# AGENTS.md

## Git 约定

- 每完成一个可提交的工作单元就自动 commit，不用询问用户
- 提交信息：英文 Conventional Commits（`feat:` / `fix:` / `docs:` …）
- 尾注固定为：`Co-authored-by: ZCode (GLM-5.3-Flash) <noreply@bigmodel.cn>`
- 仓库局部身份：`lightless233 <lightless@foxmail.com>`；分支 `master`；不主动 push

## 开发与运行

- 开发与测试用的 server 必须设置 `LIMPET_HOME=~/.limpet-dev`；默认的 `~/.limpet` 属于正式服务。跑测试脚本时带上同一个 `LIMPET_HOME`。
- 停 server 只用普通 `kill` 或 `systemctl --user`。会按进程树整棵清理的停止方式（包括 agent 工具的「停止后台任务」）会把 worker 一起带走，结束用户的会话。
- `pgrep -f` / `pkill -f` 会匹配 agent 外层 shell 的命令行（里面是整段命令文本）：测试用的标记字面量不要出现在同一条命令里，模式用 `[x]yz` 形式。
- 正式部署：`deploy/install.sh`（见 `deploy/README.md`）。服务监听 `127.0.0.1:13000`，安装目录 `/opt/limpet`，不依赖本仓库的路径。
- **上线必须由用户明确指示**：`deploy/install.sh` 会启用并启动正式服务（经 Tunnel 对外可达）。没有用户的明确指示不要执行它，也不要用任何别的方式启动正式服务。
