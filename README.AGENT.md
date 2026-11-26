# Limpet — AI Agent Guide (Deployment & Operations)

Human README: [English](./README.en.md) | [简体中文](./README.md) | Deployment Notes (zh-CN): [deploy/README.md](./deploy/README.md)

This document is written specifically for AI coding/ops agents helping a user **build, deploy, integrate, operate, or troubleshoot Limpet**. Follow every safety boundary below strictly.

---

## 1. Architecture & Binary Dispatch

Limpet compiles into a single Go binary (`./server`, requiring cgo + `libghostty-vt`) with the Vue frontend (`web/dist`) embedded via `//go:embed all:dist` when built with `-tags limpet_embed`.

`argv[0]` and first-argument dispatch ([`server/main.go`](./server/main.go)):

| Invocation | Role |
| --- | --- |
| `limpet-server` | Starts HTTP/WebSocket/control server (`LIMPET_ADDR`, default `127.0.0.1:4040`) |
| `limpet-server worker <uuid>` | Internal worker daemon holding 1 PTY, dual `libghostty` grids (`grid` + `shadow`), recording, and N viewer sockets |
| `limpet-server version` | Prints embedded build version (`ldflags -X main.version=...`, default `dev`) |
| `limpet ...` (symlink to `limpet-server`) | Dispatches CLI subcommands (`limpet`, `limpet <cmd>`, `limpet ls`, `limpet attach`, `limpet server`, `limpet version`) |

### `$LIMPET_HOME` Data Directory Layout

Default: `~/.limpet` (used by production `limpet-server.service`). Override with `LIMPET_HOME` for any dev/test run.

- `server.lock`: `flock` lock preventing two servers from sharing the same directory (registry corruption guard).
- `server.sock`: Local Unix socket (`0600`) for CLI control ops (`register`, `list`).
- `sessions.json`: Session name & metadata registry (held in memory by server and written atomically).
- `workers/<uuid>.sock` & `workers/<uuid>.log`: Per-session Unix stream sockets (`0600`) and worker logs.
- `history/<uuid>.log` & `history/<uuid>.meta`: Session output and dimension/metadata recordings.

---

## 2. Critical Safety Rules for Agents

1. **Separate dev vs production state**: Always export `LIMPET_HOME=~/.limpet-dev` (or a temporary path) for dev servers and tests. **Never** start a test server against `~/.limpet` while production is running.
2. **Never kill process trees when stopping a server**: Stop a dev server only with plain `kill <pid>`, or production via `systemctl --user stop limpet-server`. Process-tree kills (including some agent background-task abort helpers) can wipe out child worker processes and destroy the user's live terminal sessions.
3. **`pgrep -f` / `pkill -f` self-match trap**: When searching for server processes in shell commands, bracket one letter (`pgrep -f '[l]impet-server'`) and anchor carefully so you do not match your own shell wrapper or live `worker` processes.
4. **External auth required**: Limpet has **no built-in authentication**. Never bind `LIMPET_ADDR` to `0.0.0.0` or expose a port externally unless protected by Cloudflare Access, Tailscale, or an authenticating reverse proxy.
5. **Confirm before deploying production**: Running [`deploy/install.sh`](./deploy/install.sh) immediately builds, replaces `/opt/limpet/limpet-server`, and starts `limpet-server.service` (reaching external traffic if a tunnel is active). Ask the user before running it in production environments.

---

## 3. Prerequisites & Preflight Checks

Run these checks before building or deploying:

```sh
# Toolchain & runtimes
command -v go node pnpm pkg-config systemctl

# Verify libghostty-vt is discoverable (set PKG_CONFIG_PATH if custom-installed)
export PKG_CONFIG_PATH="${LIMPET_PKG_CONFIG:-$HOME/.local/opt/limpet-toolchain/install-1.0.1/usr/lib/pkgconfig}${PKG_CONFIG_PATH:+:$PKG_CONFIG_PATH}"
pkg-config --modversion --cflags --libs libghostty-vt

# Required only for systemd --user deployment
systemctl --user show-environment >/dev/null
```

If `libghostty-vt` is installed in another prefix, export `LIMPET_PKG_CONFIG=/path/to/pkgconfig` (used automatically by `deploy/install.sh`) or `PKG_CONFIG_PATH` before invoking `go build` / `go test`.

---

## 4. Build & Deployment Playbooks

### Playbook A — Local Dev / Trial Server (No systemd)

```sh
# 1. Build frontend
(cd web && pnpm install --frozen-lockfile && pnpm build)

# 2. Build embedded single binary
PKG_CONFIG_PATH="${LIMPET_PKG_CONFIG:-$HOME/.local/opt/limpet-toolchain/install-1.0.1/usr/lib/pkgconfig}${PKG_CONFIG_PATH:+:$PKG_CONFIG_PATH}" \
  go build -tags limpet_embed -o ~/.limpet-dev/limpet-server ./server
ln -sfn ~/.limpet-dev/limpet-server ~/.limpet-dev/limpet

# 3. Run server (isolated LIMPET_HOME, default direct worker launch)
nohup setsid env \
  LIMPET_HOME="$HOME/.limpet-dev" \
  LIMPET_ADDR="127.0.0.1:4040" \
  "$HOME/.limpet-dev/limpet-server" </dev/null >"$HOME/.limpet-dev/server.log" 2>&1 &

# 4. Verify
curl -sSf http://127.0.0.1:4040/api/term
```

Stop ONLY the server process (without harming workers):

```sh
kill "$(pgrep -f '/.limpet-dev/[l]impet-server$')"
```

### Playbook B — Production Install & Zero-Downtime Upgrades (`systemd --user`)

Production installs to `/opt/limpet` (default) and runs independent of the Git working tree:

1. **One-time install directory setup**:
   ```sh
   sudo mkdir -p /opt/limpet && sudo chown "$USER": /opt/limpet
   ```
2. **Ensure WSL/systemd user persistence** (needed for auto-start and survival across shell exits):
   ```sh
   loginctl enable-linger "$USER"
   ```
3. **Run installer / upgrader** (from repo root, inside a normal user shell so `PATH` captures Windows tools on WSL2 if desired):
   ```sh
   ./deploy/install.sh
   ```
   What [`deploy/install.sh`](./deploy/install.sh) does:
   - Builds `web/` (`pnpm build`);
   - Builds `go build -tags limpet_embed -ldflags "-X main.version=$version"` atomically to `/opt/limpet/limpet-server`;
   - Maintains symlink `~/.local/bin/limpet -> /opt/limpet/limpet-server` (fails safely if a non-symlink file already exists there);
   - Writes `~/.config/systemd/user/limpet-server.service` with `LIMPET_ADDR=127.0.0.1:${LIMPET_PORT:-13000}`, `LIMPET_WORKER_LAUNCH=systemd-run`, `LANG=${LANG:-C.UTF-8}`, WSLg env vars, and a `PATH` snapshot;
   - Runs `systemctl --user daemon-reload`, enables and restarts `limpet-server.service`.

4. **Upgrade behavior**:
   - Existing `limpet-worker-<uuid>.service` transient units continue running uninterrupted with their original binary.
   - New sessions spawned after restart use the new `/opt/limpet/limpet-server` binary.
   - Verify after deploy:
     ```sh
     /opt/limpet/limpet-server version
     systemctl --user is-active limpet-server.service
     curl -sSf http://127.0.0.1:13000/api/term
     ```

5. **Custom install path or port**:
   ```sh
   LIMPET_PORT=13001 LIMPET_PKG_CONFIG=/custom/pkgconfig ./deploy/install.sh /opt/custom-limpet
   ```

6. **Uninstall**:
   ```sh
   systemctl --user disable --now limpet-server
   rm -f ~/.config/systemd/user/limpet-server.service ~/.local/bin/limpet
   systemctl --user daemon-reload
   rm -rf /opt/limpet
   # ~/.limpet session history/logs are preserved unless manually deleted
   ```

### Playbook C — Reverse Proxy / Cloudflare Tunnel + Cloudflare Access

Point your tunnel or reverse proxy to `http://127.0.0.1:13000` (HTTP + WebSocket upgrade required on `/api/ws/term/*`):

- Verify Access gates unauthenticated traffic:
  ```sh
  curl -sI https://<your-domain>/api/term   # expect HTTP 302 to *.cloudflareaccess.com
  ```
- **PWA behind Cloudflare Access**: `web/index.html` already includes `<link rel="manifest" href="/manifest.json" crossorigin="use-credentials" />` so logged-in browsers attach `CF_Authorization` cookies when fetching the manifest. If a specific Android browser's background WebAPK packager still fails to fetch icons without session cookies, instruct the user to add an unauthenticated Access **Bypass** policy strictly for `/manifest.json`, `/limpet.svg`, `/limpet-192.png`, and `/limpet-512.png`.

---

## 5. CLI & API Reference for Agents

### CLI Commands

```sh
limpet                                   # Start interactive shell in cwd and attach
limpet [-n <name>] [--] <cmd> [args...]  # Start command in cwd and attach (name: <=100 chars, no ':', '.', control chars)
limpet ls                                # Tab-separated list: Name, ID, Attached, Cwd, Cmd
limpet attach <name|id>                  # Re-attach to an active session
limpet server                            # Run HTTP/WS server in foreground
limpet version                           # Print version
```

- When `limpet-server` is running, CLI sessions register asynchronously via `$LIMPET_HOME/server.sock`.
- When `limpet-server` is offline, `limpet ls` and `limpet attach` still work offline by probing `$LIMPET_HOME/workers/*.sock` directly and merging `$LIMPET_HOME/sessions.json`.
- `limpet attach` exits cleanly with reset escape sequences on `SIGHUP`/`SIGTERM` without terminating the worker.

### Environment Variables

| Variable | Default | Purpose |
| --- | --- | --- |
| `LIMPET_HOME` | `~/.limpet` | Data root (`workers/`, `history/`, `sessions.json`, `server.sock`, `server.lock`) |
| `LIMPET_ADDR` | `127.0.0.1:4040` (`127.0.0.1:13000` in `install.sh`) | HTTP/WebSocket bind address |
| `LIMPET_WORKER_LAUNCH` | `direct` (`systemd-run` in `install.sh`) | `direct` (dev) or `systemd-run` (isolated transient user units) |
| `LIMPET_PORT` | `13000` | Port written into systemd unit by `deploy/install.sh` |
| `LIMPET_PKG_CONFIG` | `~/.local/opt/limpet-toolchain/install-1.0.1/usr/lib/pkgconfig` | `PKG_CONFIG_PATH` entry used by `deploy/install.sh` |

### HTTP / WebSocket Endpoints

All non-API paths fall back to SPA `index.html` (except existing static files in `web/dist`).

- `GET /api/term`: Returns JSON array `[{ id, name, created, attached, cwd?, cmd? }]` (`cwd` folds `$HOME` to `~`; `cmd` is basename-only).
- `POST /api/term`: Body `{ "name": "..." }` (optional) -> creates session `{ id, name, created, attached }`.
- `POST /api/term/{id}/rename`: Body `{ "name": "..." }`.
- `POST /api/term/{id}/close`: Terminates session worker tree and removes socket.
- `GET /api/ws/term/{id}`: WebSocket stream.
  - Binary frames: raw terminal PTY output (server -> client) and input bytes (client -> server).
  - Text frames (JSON):
    - Server -> client: `ready` (with optional `cols`, `rows`), `size` (`cols`, `rows`), `ping`
    - Client -> server: `resize` (`cols`, `rows`; explicit `0, 0` enters Lock Width mode without changing remote PTY dimensions or claiming size ownership), `pong`

---

## 6. Daily Operations & Troubleshooting

| Task / Symptom | Command / Cause & Fix |
| --- | --- |
| Service status & logs | `systemctl --user status limpet-server` / `journalctl --user -u limpet-server -f` |
| Worker logs | `~/.limpet/workers/<uuid>.log` |
| Go tests (with race detector) | `PKG_CONFIG_PATH=... go test -race -count=1 ./server` |
| Lock Width disabled on a session | That session was spawned by an older pre-upgrade worker (missing `ready.cols/rows`); newly started sessions support Lock Width automatically |
| Missing new Windows CLI in WSL Web shell | Re-run `./deploy/install.sh` from a normal WSL terminal whose `PATH` includes the new Windows directories |
| Server refuses to start: `systemd-run` check failed | Ensure `systemd --user` is active in WSL (`systemd=true` in `/etc/wsl.conf`) and `systemd-run --user /bin/true` succeeds |
| `~/.local/bin/limpet` error during `install.sh` | A regular file already occupies `~/.local/bin/limpet`; move/remove it so `install.sh` can manage the symlink |
