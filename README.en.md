# Limpet

[简体中文](./README.md) | English | [For AI Agents](./README.AGENT.md)

A lightweight, single-user persistent Web and local CLI terminal host for Linux and WSL2—designed for monitoring long-running AI coding agents, builds, and interactive shell sessions from both desktop and mobile browsers.

Built as a single Go binary with an embedded frontend (powered by [`libghostty-vt`](https://github.com/ghostty-org/ghostty) on the backend and Vue 3 + `@wterm/vue` + `@wterm/ghostty` on the frontend), with zero external database or runtime server dependencies.

---

## Features

- **Crash-resilient & upgrade-safe sessions**: Each session runs in an isolated `worker` process holding the PTY and dual VT grids (`main` + `shadow` screens). Restarting or upgrading `limpet-server` keeps existing sessions alive and reconnects seamlessly.
- **Unified Local CLI + Web multi-view**: Launch commands directly in your terminal with `limpet` or `limpet <command>`, close your local tab without killing the process, and attach back from mobile/desktop Web or `limpet attach`.
- **Lock Width mode**: Keep mobile viewers from shrinking wide desktop/CLI TUI layouts. When enabled, mobile clients send input at zero resize ownership and render the remote PTY's exact `cols×rows` with smooth 2D scrolling and a dedicated horizontal pan rail.
- **Mobile-friendly terminal & PWA**:
  - Cohesive dark obsidian palette with explicit CJK sans-serif and monospace font stacks;
  - Installable standalone PWA (`manifest.json` configured with credentialed fetch behind auth proxies);
  - Adjustable terminal font size (`10–18px`, persisted per device);
  - Scrollable mobile keybar (`Esc`, `Ctrl+C`, `Tab`, arrow keys, `Enter`, sticky `Ctrl`) plus a pinned toggle for an expandable IME/voice-safe text composer;
  - Desktop dual-pane workspace (`≥1024px`) and mobile quick session switcher;
  - Automatic WebSocket reconnect with silent-drop heartbeat watchdog and zero-duplication replay.

---

## Security Model

Limpet implements **no built-in authentication** and binds to `127.0.0.1` by default:

- Never expose Limpet directly to the public internet or untrusted networks without an authenticating proxy.
- Put it behind **Cloudflare Tunnel + Cloudflare Access**, Tailscale/WireGuard, or another reverse proxy enforcing authentication and WebSocket support.

---

## Prerequisites

- **OS**: Linux or WSL2 (`systemd --user` with linger enabled for production service management)
- **Backend build**: Go (with cgo enabled), C compiler toolchain, `pkg-config`, and [`libghostty-vt`](https://github.com/ghostty-org/ghostty) headers & library
- **Frontend build**: Node.js + `pnpm` (compile-time only; `dist/` is embedded into the Go binary via `//go:embed`)

---

## Quick Start

### 1. Local Development / Trial Run

Make sure `pkg-config` can resolve `libghostty-vt` (set `PKG_CONFIG_PATH` if installed in a custom prefix):

```sh
# 1. Build frontend assets into web/dist
cd web && pnpm install --frozen-lockfile && pnpm build && cd ..

# 2. Build single binary with embedded frontend
PKG_CONFIG_PATH="${LIMPET_PKG_CONFIG:-$HOME/.local/opt/limpet-toolchain/install-1.0.1/usr/lib/pkgconfig}${PKG_CONFIG_PATH:+:$PKG_CONFIG_PATH}" \
  go build -tags limpet_embed -o ~/.limpet-dev/limpet-server ./server
ln -sfn ~/.limpet-dev/limpet-server ~/.limpet-dev/limpet

# 3. Start local server with an isolated data directory (listens on 127.0.0.1:4040)
LIMPET_HOME=~/.limpet-dev ~/.limpet-dev/limpet-server
```

Open `http://127.0.0.1:4040` in your browser. In another terminal, launch a CLI-attached session against the same dev server:

```sh
LIMPET_HOME=~/.limpet-dev ~/.limpet-dev/limpet zsh
```

> **Note**: Always isolate dev/test runs using `LIMPET_HOME`. Never run two `limpet-server` instances against the same `~/.limpet` directory.

### 2. Production Deployment (`systemd --user`)

See [deploy/README.md](./deploy/README.md) (Chinese) or [README.AGENT.md](./README.AGENT.md) (English operational guide) for full instructions. Summary:

```sh
# One-time install directory creation
sudo mkdir -p /opt/limpet && sudo chown "$USER": /opt/limpet

# Build, install to /opt/limpet, symlink ~/.local/bin/limpet, and start on 127.0.0.1:13000
./deploy/install.sh
```

> Running `deploy/install.sh` immediately enables and starts the production service. Ensure your auth proxy / Cloudflare Access is active beforehand if routed externally.

---

## CLI Reference

`limpet` and `limpet-server` share one binary; behavior is dispatched by `argv[0]`:

| Command | Description |
| --- | --- |
| `limpet` | Start an interactive shell in the current working directory and attach |
| `limpet [-n name] [--] <cmd> [args...]` | Launch a command and attach (e.g., `limpet codex --yolo`, `limpet -n api -- npm run dev`) |
| `limpet ls` | List active sessions (`name`, `id`, `attached`, `cwd`, `cmd`) |
| `limpet attach <name\|id>` | Re-attach to an active background session |
| `limpet server` | Run HTTP/WebSocket server in the foreground |
| `limpet version` | Print embedded build version |

Closing the local terminal tab detaches cleanly via `SIGHUP` and resets host terminal modes while leaving the background worker running.

---

## Documentation

- [简体中文 README](./README.md)
- [AI Agent Guide (English)](./README.AGENT.md)
- [Deployment Notes (中文)](./deploy/README.md)

## License

[MIT](./LICENSE)
