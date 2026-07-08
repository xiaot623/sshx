# sshx

> Transparent SSH enhancement — add remote-to-local commands, auto port forwarding, and local domains to your SSH workflow. Zero side effects when you don't need them.

**sshx** is a drop-in wrapper around OpenSSH. Wrap it as `alias ssh=sshx` and your existing SSH workflow works exactly as before — every flag, config, and connection passes through verbatim. But when you connect to a host (or Docker container) with sshx-aware features enabled, you unlock a persistent, shared remote server that gives you:

- 🔄 **Reverse command bridge** — run `sshx local <cmd>` *on the remote* to execute commands on your local machine, with stdout, stderr, exit code, and stdin all propagated.
- 🔌 **Automatic port forwarding** — remote loopback listeners (e.g., a dev server on `localhost:8080`) are automatically detected and forwarded to your local machine.
- 🌐 **Local domain binding** — access forwarded ports as `<host>.<your-user>.sshx:<port>` in your local browser, no manual `-L` flags needed.
- 🐳 **Docker container support** — target running containers by name or ID: `sshx my-container`. Command bridge support works inside containers via `docker exec`.

## Why sshx?

| Without sshx | With sshx |
|---|---|
| `ssh remote` — works normally | `ssh remote` — works normally, *plus* server starts in background |
| Need to copy a file *from* local to remote mid-session | `sshx local cat ~/file.txt` — runs on local, output streams to remote |
| Dev server on `localhost:3000` inaccessible | Open `http://debian.<your-user>.sshx:3000` in your local browser after `sshx debian` |
| Multiple terminals each need their own `-L` forwards | One shared daemon, one forward, all terminals benefit |
| Forget to set up forwarding before connecting | Ports detected and forwarded automatically |

sshx is designed to be **safe to alias**. Hosts without sshx configuration are untouched — no daemon starts, no files are created, no performance overhead.

---

## Features

### 📦 Drop-in Compatibility

- `sshx [any ssh args...]` delegates to the real OpenSSH.
- All SSH flags pass through: `-F`, `-o`, `-J`, `-L`, `-R`, `-D`, `-N`, `-T`, `-V`, `-G`, `-Q`, etc.
- `~/.ssh/config` resolution is handled by OpenSSH — no reimplementation.
- Bypass with `sshx --no-wrap` or `SSHX_DISABLE=1` at any time.

### 🐳 Docker Container Target

When the target doesn't match any SSH host, sshx falls back to resolving it as a running Docker container:

- `sshx <container-name>` — opens a shell in the container via `docker exec`.
- `sshx <container-id-prefix>` — matches by container ID prefix.
- Explicit SSH targets (`user@host`, IP addresses, hostnames with dots/colons) are never treated as Docker containers.
- Command bridge support works inside containers.
- Requires `docker` CLI available on the local machine — gracefully falls back to SSH if Docker isn't found or the container isn't running.

### 🔄 Remote-to-Local Command Bridge

Run commands on your **local machine** from inside an SSH session:

```sh
# On the remote, inside an sshx-wrapped session:
sshx local cat ~/my-local-file.txt
sshx local open -a "Google Chrome" "http://localhost:3000"
sshx local pbcopy < /tmp/some-data
```

- stdout, stderr, and exit codes propagate correctly.
- stdin is sent in batch mode — pipe data in and it reaches the local command.
- Policy: a configurable deny list controls which commands are blocked.

### 🔌 Automatic Port Detection & Forwarding

When a process on the remote starts listening on `127.0.0.1` (e.g., `npm run dev` on port 3000), sshx detects it and:

1. Broadcasts the port to the local daemon.
2. Assigns the SSH target its own loopback IP.
3. Exposes a TCP proxy at the target domain, e.g. `debian.<your-user>.sshx:3000`.

The URL port is the remote port. sshx does not bind `127.0.0.1:<port>`; it binds the target's private loopback IP instead, so `debian.<your-user>.sshx:8080` and `ubuntu.<your-user>.sshx:8080` can point at different hosts at the same time. Run `sshx forward` to see the active mappings.

### 🌐 Local Domains (macOS, Linux)

- A local DNS responder on `127.0.0.1:53` resolves active target names dynamically.
- Each target domain resolves to a private loopback IP; the URL port selects the remote listener.
- On macOS, `/etc/resolver/<suffix>` is configured once (with `sudo` when needed).
- All terminals on the same host share one DNS resolver and forwarding daemon.

### 🏗️ Shared Server Architecture

- One **server daemon** per client target alias, installed under `~/.sshx_server/<uuid>` on the remote and shared by that client's concurrent SSH sessions.
- Client connects via a hidden `socket-proxy` SSH channel.
- Server manages port sniffing, forwarding state, and command bridge routing centrally.
- Server stays alive through brief disconnects, exiting after an idle timeout with no clients.

---

## Installation

### Run with npx (Recommended)

No installation required — npx fetches the latest binary on each run:

```sh
npx @hahahhh/sshx my-server
npx @hahahhh/sshx -p 2222 user@my-server hostname
```

For repeated use, install globally:

```sh
npm install -g @hahahhh/sshx
```

The npm wrapper auto-downloads the correct native binary for your platform from GitHub Releases.

### Download Binary

Download the prebuilt binary directly from [GitHub Releases](https://github.com/xiaot623/sshx/releases):

```sh
# Example: macOS arm64, latest release
curl -L -o sshx https://github.com/xiaot623/sshx/releases/latest/download/sshx-darwin-arm64
chmod +x sshx
sudo mv sshx /usr/local/bin/sshx
```

Available binaries: `sshx-darwin-arm64`, `sshx-darwin-amd64`, `sshx-linux-arm64`, `sshx-linux-amd64`.

### Shell Alias (Recommended)

Add to your `~/.bashrc` or `~/.zshrc`:

```sh
alias ssh=sshx
```

The alias is safe — unmatched hosts have zero overhead and zero side effects.

---

## Quick Start

### 1. Connect normally

```sh
sshx my-server
sshx my-server uname -s
sshx -p 2222 user@my-server hostname
```

All existing SSH options work — `-F`, `-o`, `-J`, `ProxyJump`, etc. are handled by OpenSSH.

### 1a. Connect to a Docker container

```sh
# By container name
sshx my-dev-container

# By container ID prefix
sshx 4fa8bc

# Run a command directly
sshx my-container cat /etc/os-release
```

sshx detects that the target isn't an SSH host and automatically uses `docker exec`. The command bridge and other features work exactly the same inside containers.

### 2. Try the command bridge

Inside your SSH session on the remote:

```sh
sshx local uname -s
# → Darwin (your local machine's OS)
```

### 3. Start a dev server on the remote

On the remote, start a server listening on `localhost`:

```sh
python3 -m http.server 8080 --bind 127.0.0.1
```

On your **local** machine, open:

```
http://my-server.<your-user>.sshx:8080
```

No `-L` flags, no manual forwarding. Since each target gets its own loopback IP, another target can expose its own `8080` at the same time:

```sh
sshx forward
# http://my-server.<your-user>.sshx:8080 -> my-server:8080
# http://other-server.<your-user>.sshx:8080 -> other-server:8080
```

---

## Configuration Reference

`~/.sshx/config.yaml`:

```yaml
# Strict mode: if the sshx server fails, refuse the connection instead of
# falling back to plain SSH. Default: false (graceful fallback).
strict: false

features:
  # Remote-to-local command bridge (`sshx local <cmd>` on the remote)
  commandBridge: true

  # Auto-detect remote loopback TCP listeners and expose them via
  # <host>.<user>.sshx:<remote-port>.
  autoForward: true

commands:
  # Commands blocked from bridge execution.
  deny: []
```

---

## How It Works

```
┌─────────────────────────────────┐     ┌─────────────────────────────────┐
│  Your Local Machine              │     │  Remote Host                    │
│                                  │     │                                 │
│  Terminal A ── SSH ─────────────┼─────┼── sshx server (shared daemon)   │
│  Terminal B ── SSH ─────────────┼─────┼──   │                          │
│  Terminal C ── SSH ─────────────┼─────┼──   ├── Port sniffing           │
│                                  │     │     ├── Port forwarding         │
│  sshx local daemon               │     │     ├── Command bridge routing  │
│    ├─ Port proxy (shared)        │     │     └── Socket-proxy endpoint  │
│    └─ DNS responder (127.0.0.1)  │     │                                 │
└─────────────────────────────────┘     └─────────────────────────────────┘
```

1. **Connection**: `sshx remote` opens a normal SSH session and starts (or connects to) the client-target `sshx server` under `~/.sshx_server/<uuid>`.
2. **Bridge channel**: A hidden `socket-proxy` SSH channel links the local daemon to the remote server.
3. **Port sniffing**: The server reads `/proc/net/tcp*` (Linux) to detect loopback listeners.
4. **Forwarding**: Detected ports are forwarded through a single shared local daemon using `ssh -W`.
5. **Domains**: The local DNS responder maps `<target>.<suffix>` → localhost. The browser's URL port selects the local forwarded port.

When `sshx` is invoked for a **non-matching host** (no sshx config, or host not in scope), it first checks if the target resolves to a running Docker container. If neither SSH nor Docker matches, it `exec`s the real `ssh` directly — no daemon, no installation, no overhead.

---

## Safety & Bypass

- `sshx --no-wrap ...` — skip all sshx behavior and call raw `ssh`.
- `SSHX_DISABLE=1 sshx ...` — same as `--no-wrap`, useful in scripts.
- `sshx local ...` on a **client** (not inside a remote session) — errors immediately with a clear message. `local` is globally reserved.
- Docker containers that aren't running or can't be reached are pure passthrough — sshx falls back to raw `ssh` with no side effects.
- Unmatched hosts are pure passthrough — no files created, no processes started.

---

## Platform Support

| Platform | Client | Server | Docker Client |
|---|---|---|---|
| macOS | ✅ | — | ✅ |
| Linux | ✅ | ✅ | ✅ |
| Windows | 🔜 | — | 🔜 |

- **Client**: macOS and Linux are fully supported.
- **Server**: Linux is required for the remote sshx server (uses `/proc/net/tcp*` for port detection).
- **Docker Client**: macOS and Linux — targets any running Docker container via `docker exec`.

---

## Project Structure

```
sshx/
├── cmd/sshx/          # Main entry point
├── internal/
│   ├── cli/           # CLI parsing, host detection, Docker container resolution
│   ├── sshcompat/     # SSH argument compatibility
│   ├── config/        # YAML configuration
│   ├── protocol/      # Client-server wire protocol
│   ├── bridge/        # Command bridge (remote → local execution)
│   ├── ports/         # Port sniffing (/proc/net/tcp*)
│   ├── forward/       # TCP forwarding
│   ├── domain/        # DNS resolver
│   └── locald/        # Local daemon (socket, DNS, forwarding)
├── scripts/           # Integration tests
├── go.mod
├── go.sum
└── README.md
```

---

## Roadmap

- [x] **v1** — Command bridge (non-interactive), auto port forwarding, domain binding, shared server
- [ ] **v2** — Streaming stdin for command bridge, remote FUSE mounting, GitHub binary releases, Windows client support

---

## License

MIT
