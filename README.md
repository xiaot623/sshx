# sshx

> Transparent SSH enhancement — add remote-to-local commands, auto port forwarding, and local domains to your SSH workflow. Zero side effects when you don't need them.

**sshx** is a drop-in wrapper around OpenSSH. Wrap it as `alias ssh=sshx` and your existing SSH workflow works exactly as before — every flag, config, and connection passes through verbatim. But when you connect to a host with sshx-aware features enabled, you unlock a persistent, shared remote server that gives you:

- 🔄 **Reverse command bridge** — run `sshx local <cmd>` *on the remote* to execute commands on your local machine, with stdout, stderr, exit code, and stdin all propagated.
- 🔌 **Automatic port forwarding** — remote loopback listeners (e.g., a dev server on `localhost:8080`) are automatically detected and forwarded to your local machine.
- 🌐 **Local domain binding** — access forwarded ports as `<host>.<your-user>.sshx:<port>` in your local browser, no manual `-L` flags needed.

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
2. Creates a shared TCP forward over SSH.
3. Binds the SSH target domain, e.g. `debian.<your-user>.sshx`.

sshx first tries to use the same local port as the remote listener. If that local port is already occupied, it automatically tries the next port (`+1`) until it finds a free one. Run `sshx forward` to see the active mapping.

### 🌐 Local Domains (macOS, Linux)

- A local DNS responder on `127.0.0.1:53` resolves `*.sshx` names dynamically.
- The domain resolves to localhost; the URL port selects the forwarded local listener.
- On macOS, `/etc/resolver/<suffix>` is configured once (with `sudo` when needed).
- All terminals on the same host share one DNS resolver and forwarding daemon.

### 🏗️ Shared Server Architecture

- One **server daemon** per remote host per user, shared across all concurrent SSH sessions.
- Client connects via a hidden `socket-proxy` SSH channel.
- Server manages port sniffing, forwarding state, and command bridge routing centrally.
- Server stays alive through brief disconnects, exiting after an idle timeout with no clients.

---

## Installation

### From Source

```sh
git clone https://github.com/OWNER/sshx.git
cd sshx
go build -o ./bin/sshx ./cmd/sshx
```

Copy the binary to a location in your `$PATH`:

```sh
cp ./bin/sshx /usr/local/bin/sshx
```

### Shell Alias (Recommended)

Add to your `~/.bashrc` or `~/.zshrc`:

```sh
alias ssh=sshx
```

The alias is safe — unmatched hosts have zero overhead and zero side effects.

---

## Quick Start

### 1. Create your config

```sh
mkdir -p ~/.sshx
```

`~/.sshx/config.yaml`:

```yaml
features:
  commandBridge: true
  ports:
    auto: true
  domains:
    enabled: true

commands:
  deny: []
```

### 2. Connect normally

```sh
sshx my-server
sshx my-server uname -s
sshx -p 2222 user@my-server hostname
```

All existing SSH options work — `-F`, `-o`, `-J`, `ProxyJump`, etc. are handled by OpenSSH.

### 3. Try the command bridge

Inside your SSH session on the remote:

```sh
sshx local uname -s
# → Darwin (your local machine's OS)
```

### 4. Start a dev server on the remote

On the remote, start a server listening on `localhost`:

```sh
python3 -m http.server 8080 --bind 127.0.0.1
```

On your **local** machine, open:

```
http://my-server.<your-user>.sshx:8080
```

No `-L` flags, no manual forwarding.

If local port `8080` is already occupied, sshx will try `8081`, then `8082`, and so on. Check the chosen port with:

```sh
sshx forward
# 8080 -> my-server:8080
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

  ports:
    # Auto-detect loopback TCP listeners on the remote and forward them.
    auto: true
    # Future: also detect 0.0.0.0 listeners.
    # bindAll: false

  domains:
    # Enable local domain binding (<host>.<user>.sshx:<port>).
    enabled: true
    # Custom domain suffix. Default: <local-user>.sshx
    suffix: user.sshx

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

1. **Connection**: `sshx remote` opens a normal SSH session and starts (or connects to) a shared `sshx server` on the remote.
2. **Bridge channel**: A hidden `socket-proxy` SSH channel links the local daemon to the remote server.
3. **Port sniffing**: The server reads `/proc/net/tcp*` (Linux) to detect loopback listeners.
4. **Forwarding**: Detected ports are forwarded through a single shared local daemon using `ssh -W`.
5. **Domains**: The local DNS responder maps `<target>.<suffix>` → localhost. The browser's URL port selects the local forwarded port.

When `sshx` is invoked for a **non-matching host** (no sshx config, or host not in scope), it `exec`s the real `ssh` directly — no daemon, no installation, no overhead.

---

## Safety & Bypass

- `sshx --no-wrap ...` — skip all sshx behavior and call raw `ssh`.
- `SSHX_DISABLE=1 sshx ...` — same as `--no-wrap`, useful in scripts.
- `sshx local ...` on a **client** (not inside a remote session) — errors immediately with a clear message. `local` is globally reserved.
- Unmatched hosts are pure passthrough — no files created, no processes started.

---

## Platform Support

| Platform | Client | Server |
|---|---|---|
| macOS | ✅ | — |
| Linux | ✅ | ✅ |
| Windows | 🔜 | — |

- **Client**: macOS and Linux are fully supported.
- **Server**: Linux is required for the remote sshx server (uses `/proc/net/tcp*` for port detection).

---

## Project Structure

```
sshx/
├── cmd/sshx/          # Main entry point
├── internal/
│   ├── cli/           # CLI parsing, host detection
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
