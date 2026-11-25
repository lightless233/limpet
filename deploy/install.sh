#!/bin/sh
# Limpet 的发布构建与安装（spec 005）。在开发仓库里执行，产物装进独立目录——
# 服务运行时不依赖这个仓库，也不依赖 git 远程。
#
#   deploy/install.sh [安装目录]        默认 /opt/limpet
#
# 步骤：构建前端 → 发布构建（内嵌前端、写入版本）→ 原子替换安装目录里的二进制 →
#       生成单元文件 → 链接进 systemd 用户目录 → daemon-reload → 启用并重启服务。
# 服务监听 127.0.0.1:${LIMPET_PORT:-13000}，数据目录是 ~/.limpet。重启服务不影响已有会话
# （worker 在各自的 systemd 单元里）。详见 deploy/README.md。
set -eu

repo=$(cd "$(dirname "$0")/.." && pwd)
install_dir=${1:-/opt/limpet}
port=${LIMPET_PORT:-13000}
unit_dir="$HOME/.config/systemd/user"
unit=limpet-server.service

fail() {
  echo "安装失败：$*" >&2
  exit 1
}
command -v go >/dev/null 2>&1 || fail "缺少 go"
command -v pnpm >/dev/null 2>&1 || fail "缺少 pnpm"
systemctl --user show-environment >/dev/null 2>&1 || fail "systemd 用户管理器不可用"
if [ ! -d "$install_dir" ]; then
  fail "安装目录 $install_dir 不存在。创建并交给当前用户（只需一次）：
    sudo mkdir -p $install_dir && sudo chown \$USER: $install_dir"
fi
[ -w "$install_dir" ] || fail "安装目录 $install_dir 对当前用户不可写（权限见上一条提示）"

# libghostty-vt 的 .pc 不在 pkg-config 的默认搜索路径里；不带上它，构建缓存一失效 go build 就失败。
: "${LIMPET_PKG_CONFIG:=$HOME/.local/opt/limpet-toolchain/install-1.0.1/usr/lib/pkgconfig}"
export PKG_CONFIG_PATH="$LIMPET_PKG_CONFIG${PKG_CONFIG_PATH:+:$PKG_CONFIG_PATH}"

echo "→ 构建前端"
(cd "$repo/web" && pnpm build >/dev/null)

echo "→ 发布构建（内嵌前端）"
version=$(git -C "$repo" describe --always --dirty 2>/dev/null || echo unknown)
tmp="$install_dir/.limpet-server.tmp"
(cd "$repo" && go build -tags limpet_embed -ldflags "-X main.version=$version" -o "$tmp" ./server)
chmod 755 "$tmp"
mv "$tmp" "$install_dir/limpet-server"

echo "→ 生成单元文件"
# WSL 会话基线：systemd 不继承 WSL 会话环境，而缺 UTF-8 locale 时会话里的中文会出错。
lang=${LANG:-C.UTF-8}
# Windows 目录取自当前 PATH（安装时的快照，去重、保持顺序）；装了新的 Windows 工具后重新执行本脚本即可。
win_path=$(printf '%s\n' "$PATH" | tr ':' '\n' | grep -E '^/mnt/[a-zA-Z]/' | awk '!seen[$0]++' | paste -sd: - | sed 's/%/%%/g')
{
  cat <<UNIT
# 由 deploy/install.sh 生成（spec 005）。要改配置请改仓库里的脚本，然后重新执行安装。
[Unit]
Description=Limpet web terminal server
After=network.target

[Service]
Type=exec
WorkingDirectory=$install_dir
ExecStart=$install_dir/limpet-server
Environment=LIMPET_ADDR=127.0.0.1:$port
# worker 必须经 systemd-run 进入自己的单元；留在本单元的控制组里会随服务停止被一并清理（spec 003）。
Environment=LIMPET_WORKER_LAUNCH=systemd-run
Environment=LANG=$lang
UNIT
  if [ -n "${WSL_DISTRO_NAME:-}" ]; then echo "Environment=WSL_DISTRO_NAME=$WSL_DISTRO_NAME"; fi
  cat <<UNIT
Environment=WSL2_GUI_APPS_ENABLED=1
Environment=DISPLAY=:0
Environment=WAYLAND_DISPLAY=wayland-0
Environment=PULSE_SERVER=unix:/mnt/wslg/PulseServer
# 基础 PATH：系统目录在前、Windows 目录在后（与 WSL 的默认顺序一致）；Windows 目录是安装时的快照。
Environment="PATH=/usr/lib/wsl/lib:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin${win_path:+:$win_path}"
Restart=on-failure
RestartSec=2

[Install]
WantedBy=default.target
UNIT
} >"$install_dir/$unit.tmp"
mv "$install_dir/$unit.tmp" "$install_dir/$unit"

echo "→ 注册并启动服务"
mkdir -p "$unit_dir"
ln -sfn "$install_dir/$unit" "$unit_dir/$unit"
systemctl --user daemon-reload
systemctl --user enable limpet-server.service >/dev/null 2>&1 || fail "启用服务失败"
systemctl --user restart limpet-server.service || true
sleep 1
if ! systemctl --user is-active --quiet limpet-server.service; then
  journalctl --user -u limpet-server -n 20 --no-pager >&2 || true
  fail "服务没有进入 active 状态（上方是最近的日志）"
fi
echo "→ 已上线：limpet-server $version，监听 127.0.0.1:$port，数据目录 ~/.limpet"
echo "   日志：journalctl --user -u limpet-server -f"
