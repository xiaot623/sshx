# sshx

> Transparent SSH enhancement — add remote-to-local commands, auto port forwarding, and local domains to your SSH workflow. Zero side effects when you don't need them.

**sshx** is a drop-in wrapper around OpenSSH. Wrap it as `alias ssh=sshx` and your existing SSH workflow works exactly as before — every flag, config, and connection passes through verbatim. But when you connect to a host (or Docker container) with sshx-aware features enabled, you unlock a persistent, shared remote server that gives you:

- 🔄 **Reverse command bridge** — run `sshx local <cmd>` *on the remote* to execute commands on your local machine, with stdout, stderr, exit code, and stdin all propagated.
- 📁 **Bidirectional home mount** — opt in to mount the command initiator's home while preserving the source path hierarchy and working directory.
- 🔌 **Automatic port forwarding** — remote local listeners (loopback `127.0.0.1` and wildcard `0.0.0.0`; e.g., a dev server on `0.0.0.0:8080` or `localhost:8080`) are automatically detected and forwarded to your local machine.
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
sshx local --timeout=30 npm test
```

- stdout, stderr, and exit codes propagate correctly.
- stdin is sent in batch mode — pipe data in and it reaches the local command.
- Commands have no implicit deadline. Put `--timeout=<duration>` immediately after the target to opt in; bare numbers mean seconds, and values such as `500ms`, `30s`, and `2m` are accepted. Timed-out commands exit with status 124.
- Policy: a configurable deny list controls which commands are blocked.

### 📁 Bidirectional Workspace Mount (opt-in, beta)

Set `features.remoteFs: true` to expose the command initiator's home directory through a read-write FUSE mount. Its absolute hierarchy is preserved below the managed session directory (for example, `/Users/xiaot` becomes `<session>/Users/xiaot`):

- `sshx remote <cmd>` starts the remote command at the corresponding path inside the mounted local home.
- An interactive `sshx remote` shell still starts in the remote home. `SSHX_MOUNT_ROOT` points to the mounted source root and `SSHX_WORKSPACE` points to the mapped source working directory.
- From that shell, `sshx local <cmd>` mounts the remote home locally and starts the local command at the corresponding mapped path.
- If the source working directory is outside its home, sshx exports that directory as a safe fallback and still preserves its absolute hierarchy below the session directory.
- Writes and `fsync` are sent to the source immediately. Metadata and directory entries use short TTLs and are revalidated when files are reopened.

The mounted home permits reads, writes, and creation, but blocks file/directory deletion and rename in both directions. It can still include sensitive files such as shell configuration and SSH credentials, so enable `remoteFs` only for targets you trust. sshx excludes its managed mount tree from reverse exports to avoid recursive mounts.

Set `FS_READ_ONLY=1` on the client when starting sshx to make the session mounts read-only:

```sh
FS_READ_ONLY=1 sshx debian@orb pwd
```

The client value is exported into the remote session. A later `sshx local <cmd>` inherits it, so the reverse mount is read-only as well.

FUSE is a hard dependency when this feature is enabled: a mount failure aborts the command even when `strict` is false. Linux needs `/dev/fuse` plus `fusermount`/`fusermount3`; macOS needs a current macFUSE installation and is currently beta. The remote target must be Linux with FUSE available.

#### FUSE setup

The machine receiving the mounted view needs a working FUSE runtime. The Linux target therefore always needs FUSE for `sshx remote`; a macOS client also needs macFUSE when a remote shell runs `sshx local` and mounts the remote working directory back on the Mac.

**Linux target/client**

Install the FUSE 3 userspace tools (the kernel normally already includes the FUSE driver):

```sh
# Debian / Ubuntu
sudo apt-get update && sudo apt-get install -y fuse3

# Fedora / RHEL-family
sudo dnf install -y fuse3

# Arch Linux
sudo pacman -S fuse3
```

Verify both the device and unmount helper:

```sh
test -r /dev/fuse && test -w /dev/fuse
command -v fusermount3 || command -v fusermount
```

If `/dev/fuse` is missing on a normal Linux host, load the kernel module with `sudo modprobe fuse`. Containers and restricted VMs must also expose `/dev/fuse` and permit FUSE mounts; installing `fuse3` alone is not sufficient. Docker targets are not supported by `remoteFs` yet.

**macOS client (current sshx backend)**

Install the latest macFUSE release from [macfuse.io](https://macfuse.io/) (recommended by the macFUSE project) or with `brew install --cask macfuse`. The current sshx implementation uses macFUSE's kernel/VFS backend.

On Apple Silicon, first-time kernel-backend setup requires:

1. Shut down, then hold the power/Touch ID button to enter macOS Recovery.
2. Open Startup Security Utility, select the system volume, and choose **Reduced Security**.
3. Enable **Allow user management of kernel extensions from identified developers**, then restart.
4. In **System Settings → Privacy & Security**, allow the macFUSE system software when prompted, then restart again.

Intel Macs do not need the Startup Security Utility change, but may still require approving macFUSE in Privacy & Security and restarting. macFUSE does not require disabling SIP or Gatekeeper.

After approval, trigger a mount once and verify that macFUSE loaded:

```sh
ls /Library/Filesystems/macfuse.fs
ls /dev/macfuse*
```

**macOS 15.4+ FSKit note:** macFUSE 5 provides a userspace FSKit backend that does not require a kernel extension, Recovery-mode security changes, or a restart. It is not transparent to the current sshx mount implementation and is not enabled yet: macFUSE requires the explicit `-o backend=fskit` option, FSKit only supports mount points below `/Volumes`, and several traditional mount options are unavailable. sshx currently creates private mounts below the runtime temporary directory and supplies VFS-oriented options. Supporting FSKit therefore requires a dedicated mount-path/options adapter, although the RemoteFS wire protocol and file-operation backend can remain unchanged.

Absolute source paths are preserved as a hierarchy below sshx's private session directory, but absolute command arguments are not rewritten. RemoteFS does not expose special files/xattrs/ACLs or support Docker targets, FUSE-T, or FSKit. It is optimized for source trees and small files rather than large-file throughput.

### 🔌 Automatic Port Detection & Forwarding

When a process on the remote starts listening on `127.0.0.1` or `0.0.0.0` (e.g., `npm run dev` on port 3000), sshx detects it and:

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
- Clients renew local and remote leases every 5 seconds. A daemon expires a client after 15 seconds without a heartbeat.
- The local daemon exits when its last client lease closes. The remote server drains briefly and exits after its last bridge lease closes.
- Application or protocol version changes drain the existing daemon before the current binary starts a replacement.

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
sshx my-server --timeout=30 npm test
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

# Stop a command after 30 seconds (bare numbers are seconds)
sshx my-container --timeout=30 npm test
```

sshx detects that the target isn't an SSH host and automatically uses `docker exec`. The command bridge and other features work exactly the same inside containers.

### 2. Try the command bridge

Inside your SSH session on the remote:

```sh
sshx local uname -s
# → Darwin (your local machine's OS)

# Long-running bridge commands have no implicit deadline; opt in when needed
sshx local --timeout=30 npm test
```

### 3. Start a dev server on the remote

On the remote, start a server:

```sh
python3 -m http.server 8080
```

`python3 -m http.server` binds to `0.0.0.0` by default; sshx detects it the same as a `--bind 127.0.0.1` listener. Explicit `--bind <ip>` to a non-loopback interface is not forwarded.

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

  # Auto-detect remote loopback and wildcard TCP listeners and expose them via
  # <host>.<user>.sshx:<remote-port>.
  autoForward: true

  # Read-write workspace mounts in both command directions. Default: false.
  # Requires FUSE on the local machine and remote target.
  remoteFs: false

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
2. **Bridge channels**: A hidden control `socket-proxy` channel links the client to the remote server. `remoteFs` adds a separately framed, bounded data channel paired by session ID.
3. **Port sniffing**: The server reads `/proc/net/tcp*` (Linux) to detect loopback (`127.0.0.1` / `::1`) and wildcard (`0.0.0.0` / `::`) listeners.
4. **Forwarding**: Detected ports are forwarded through a single shared local daemon using `ssh -W`.
5. **Domains**: The local DNS responder maps `<target>.<suffix>` → localhost. The browser's URL port selects the local forwarded port.

When `sshx` is invoked for a **non-matching host** (no sshx config, or host not in scope), it first checks if the target resolves to a running Docker container. If neither SSH nor Docker matches, it `exec`s the real `ssh` directly — no daemon, no installation, no overhead.

---

## Safety & Bypass

- `sshx --no-wrap ...` — skip all sshx behavior and call raw `ssh`.
- `SSHX_DISABLE=1 sshx ...` — same as `--no-wrap`, useful in scripts.
- `sshx local ...` on a **client** (not inside a remote session) — errors immediately with a clear message. `local` is globally reserved.
- `remoteFs` never silently falls back to an unmounted command. A failed FUSE mount fails the invocation.
- Workspace exports are anchored with Go's `os.Root`; path traversal and symlink escapes are rejected.
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
- **remoteFs**: Linux is supported; macOS clients require macFUSE and are beta. Docker targets are not supported.

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
│   ├── remotefs/      # FS protocol, secure backend, and FUSE adapter
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
- [ ] **v2** — Streaming stdin for command bridge, GitHub binary releases, Windows client support
- [x] **remoteFs beta** — Bidirectional read-write workspace mounting on Linux/macOS clients and Linux targets

---

## License

MIT
