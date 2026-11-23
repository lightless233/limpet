# Limpet 正式安装

开发仓库只用于开发。正式运行的产物装在独立目录（默认 `/opt/limpet`），里面只有两样：

- `limpet-server`：发布二进制。前端页面内嵌其中，带版本号（`limpet-server version`）。
- `limpet-server.service`：systemd 用户级服务单元。

服务运行时不依赖仓库路径，也不依赖 git 远程。

## 首次安装

1. 创建安装目录（只需一次，需要 root）：

   ```sh
   sudo mkdir -p /opt/limpet && sudo chown $USER: /opt/limpet
   ```

2. 上线前确认 Tunnel 域名在 Cloudflare Access 之后。Limpet 自身不做认证，全靠 Access 挡在前面：

   ```sh
   curl -sI https://<域名>/api/term    # 期望 302，Location 指向 *.cloudflareaccess.com
   ```

3. 在仓库根目录执行 `deploy/install.sh`。脚本依次完成：构建前端 → 发布构建 → 替换二进制 →
   生成单元文件 → 启用并（重新）启动服务。

## 升级

在仓库里更新代码后再次执行 `deploy/install.sh`。运行中的会话不受影响：worker 是独立进程，继续用
旧的二进制；此后新建的会话用新的。

查看线上版本：`/opt/limpet/limpet-server version`。

## 日常操作

| 操作 | 命令 |
| --- | --- |
| 状态 | `systemctl --user status limpet-server` |
| 停止 / 启动（不影响已有会话） | `systemctl --user stop limpet-server` / `systemctl --user start limpet-server` |
| 重启 | `systemctl --user restart limpet-server` |
| 日志 | `journalctl --user -u limpet-server -f` |
| 单个 worker 的日志 | `~/.limpet/workers/<uuid>.log` |
| 会话列表 | `curl -s 127.0.0.1:13000/api/term` |

WSL 启动后服务自启（依赖已启用的 linger）。

## 端口

默认监听 `127.0.0.1:13000`。改端口：`LIMPET_PORT=端口 deploy/install.sh`，Tunnel 路由要同步改。

## 会话里的环境

systemd 不继承 WSL 会话环境，所以单元文件里由安装脚本写入一份基线：

- `LANG`：取安装时的值（没有则 `C.UTF-8`）。缺 UTF-8 locale 时会话里的中文会出错。
- `WSL_DISTRO_NAME` 与 WSLg 变量（`DISPLAY`、`WAYLAND_DISPLAY`、`PULSE_SERVER`）。
- 基础 `PATH`：系统目录在前，安装时 PATH 里的 Windows 目录在后。

用户自己的 CLI（node、pnpm、codex 等）的路径由 Shell 的配置文件提供，不受影响。

Windows 目录是一份快照。装了新的 Windows 工具后 `explorer.exe`、`code` 之类按名字找不到时，
**在一个正常的 WSL 终端里**重新执行 `deploy/install.sh` 即可刷新。安装时所在的终端 PATH 里
没有 Windows 目录，快照里也就不会有。

## 数据目录

- `~/.limpet`：正式服务的会话数据（socket、录制、注册表）。
- 开发与测试必须用别的数据目录：启动 server 时设置 `LIMPET_HOME=~/.limpet-dev`。
  两个 server 不能共用一个注册表——它由 server 在内存里持有并整份写回，互相覆盖会丢会话名。

## 卸载

```sh
systemctl --user disable --now limpet-server
rm ~/.config/systemd/user/limpet-server.service
systemctl --user daemon-reload
rm -rf /opt/limpet
```

会话数据 `~/.limpet` 不会被动到；确认不再需要时自行删除。
