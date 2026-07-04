# sshx Transparent Proxy Plan

## Overview

sshx is an OpenSSH-compatible wrapper with zero side effects for unmatched invocations. Users can safely set `alias ssh=sshx`.

Core command usage:

```
sshx remote
sshx root@192.168.1.100
sshx remote uname -a        # Run command on remote locally, equivalent to ssh
sshx local uname -a         # Errors immediately on the client — "local" is reserved
                             # Only valid inside a remote wrapper session:
                             # The remote executes the command on the client via the sshx bridge
```

`local` is a **globally reserved target name** across the entire sshx binary — on both client and remote. Running `sshx local` on the client immediately produces a clear error: `"local" is a reserved name for the remote-to-local command bridge, use it inside an sshx remote session.` It will never fall back to OpenSSH. This avoids ambiguity when users have a `Host local` in their `.ssh/config`.

`sshx <host> <cmd>` retains ordinary SSH semantics. `sshx local <cmd>` is the reserved command bridge target, available only within an sshx-created remote wrapper context.

## Core Behavior

### Ordinary SSH Invocations

- `sshx [ssh args...]` delegates the raw argv to the real OpenSSH.
- All OpenSSH functionality remains available: `~/.ssh/config`, `Host`, `ProxyJump`, remote commands, TTY flags, forwarding flags, `-F`, `-o`, `-G`, `-V`, etc.
- If no wrapper rules are matched, sshx directly execs ssh — no daemon is started, no installation occurs, no injection happens, and no configuration is modified.

### Matched SSH Hosts

- sshx connects to a **shared Server daemon** on the remote host (one Server per user per remote host, shared across all client terminals).
- If no Server is running, the first client installs and starts it.
- The user-visible SSH session remains an ordinary OpenSSH connection.
- The Server centrally manages binary installation, the command bridge, remote port sniffing, forwarding, and local domain routing — all client sessions share this state.
- On Server exception, fall back to ordinary SSH (unless `strict: true` is configured).

### Remote-to-Local Commands

- The remote Server binary supports `sshx local <cmd...>`.
- It sends command execution requests to connected clients via the active Server session.
- If no client is connected or no active bridge session exists, it reports a clear error.
- Local commands execute as the client user, without privilege escalation.
- v1 supports **non-interactive commands only** (no PTY is allocated for bridge commands). stdin uses batch mode — all input is collected before the command starts.

## Architecture: Single Server, Multiple Clients

sshx uses one long-running Server daemon per remote host, shared by all concurrent client terminals.

```
┌──────────────────────────────┐     ┌──────────────────────────────┐
│  Client Machine               │     │  Remote Host                  │
│                              │     │                              │
│  Terminal A ── SSH ──────────┼─────┼── sshx server (shared)        │
│  Terminal B ── SSH ──────────┼─────┼──   │                        │
│  Terminal C ── SSH ──────────┼─────┼──   ├── Port sniffing         │
│                              │     │     ├── Port forwarding        │
│  sshx local daemon            │     │     ├── Command bridge         │
│    ├── Port proxy             │     │     └── Domain routing         │
│    ├── Domain resolution      │     │                              │
│    └── Bridge endpoint        │     │                              │
└──────────────────────────────┘     └──────────────────────────────┘
```

- **Server lifecycle**: The first matched `sshx <host>` starts the Server on the remote. The Server stays alive after clients disconnect (configurable idle timeout, e.g., exit after 10 minutes with no connected clients).
- **Port forwarding**: Centrally managed by the Server — no port conflicts across concurrent terminals.
- **Discovery**: The Server writes connection info to `~/.sshx/server-info` (Unix socket path or localhost port + auth token). Each new client reads this file to connect.

## Implementation Details

### SSH Compatibility Layer

- Unrecognized flags are treated as OpenSSH flags.
- Parses only enough to detect the `local` target, `--no-wrap` flag, and potential host matches.
- Uses OpenSSH itself for parsing when needed, especially `ssh -G`.
- Info/query modes like `-V`, `-G`, `-Q` pass through verbatim with zero side effects.
- The `--no-wrap` flag skips all sshx logic and directly execs OpenSSH. It overrides the `SSHX_DISABLE` environment variable.
- The environment variable `SSHX_DISABLE=1` likewise bypasses wrapping (for scripting scenarios).

### Server Protocol

A framed protocol between Client and Server, transmitted over stdio through the SSH control channel. Required message types:

| Message | Direction | Purpose |
|---------|-----------|---------|
| `hello` | client → server | Handshake: protocol version, capability set |
| `capabilities` | server → client | Broadcast supported features |
| `command.exec` | server → client | Remote requests execution of a local command |
| `command.result` | client → server | Command output, exit code |
| `command.error` | client → server | Command failed to execute on the client |
| `port.observed` | server → client | New listening port detected on the remote |
| `port.forward` | client → server | Request port forwarding |
| `error` | bidirectional | Error reporting |
| `heartbeat` | bidirectional | Keep-alive |

#### Command Execution Frames (v1 Batch stdin)

```json
// Server → Client: execute command locally
{
  "type": "command.exec",
  "id": "req-1",
  "argv": ["uname", "-a"],
  "env": {"HOME": "/home/user"},
  "cwd": "/home/user",
  "stdin": "base64encoded..."
}

// Client → Server: command execution result
{
  "type": "command.result",
  "id": "req-1",
  "exitCode": 0,
  "stdout": "base64...",
  "stderr": "base64..."
}

// Client → Server: command failed on the client
{
  "type": "command.error",
  "id": "req-1",
  "error": "executable not found"
}
```

v1 uses batch stdin: all stdin data is collected and sent before the command starts, avoiding the complexity of protocol-level streaming stdin. v2 will implement true streaming via `stdin.data` / `stdin.eof` / `stdin.close` frames.

### Command Bridge

- **Local to remote**: `sshx host cmd` delegates to OpenSSH — unchanged.
- **Remote to local**: `sshx local cmd` on the remote sends a `command.exec` request to connected clients via the active Server session.
- **Policy**: A deny list in the configuration file controls which commands are blocked from bridging. v1 default: allowed within authenticated Server sessions, non-interactive commands only.
- **Stream preservation**: stdout, stderr, and exit status propagate correctly.
- **Signal / cancellation**: The client can cancel a running command (v2, not v1).

### Remote Installation

- The first matched connection uploads the Go binary to `~/.sshx/bin/sshx`.
- Per-user installation, no root required.
- Protocol version and checksum are verified before reuse.
- Installation/update only occurs for hosts matching wrapper rules.
- **v1**: Manual binary installation for testing. Production distribution via GitHub releases (v2).

### Ports & Domains

- The remote Server only sniffs TCP listening ports on `127.0.0.1` / `::1` by default. `0.0.0.0` ports are not sniffed without explicit configuration.
- The Server sends a `port.observed` message when a new port is detected.
- The local daemon creates forwardings and domain routes.
- Domain suffix: `${user}.sshx` (e.g., `xiaot.sshx`). Avoids `.local` which conflicts with mDNS/Bonjour.
- One-time local DNS resolver setup (e.g., on macOS, `/etc/resolver/xiaot.sshx` pointing to `127.0.0.1:53` where the local DNS proxy listens).
- Dynamic names resolve to localhost; the URL port selects the local forwarded listener.
- Port forwarding conflicts are naturally avoided since all forwarding is managed by one shared Server per remote host.

### Mounting (Deferred to v2)

- Not in scope for v1.
- Future: local-to-remote directory mounting via remote FUSE, with filesystem operations proxied through the Server connection.

## Server Discovery

The Server writes connection endpoint info to `~/.sshx/server-info` on the remote host. Format:

```json
{
  "protocol": "unix",
  "address": "/home/xiaot/.sshx/sock",
  "token": "abc123..."
}
```

Discovery flow:

1. The client opens an SSH session to the remote host.
2. The client reads `~/.sshx/server-info` to check whether a Server exists.
3. If a Server is running and reachable: the client connects to it.
4. If not: the client installs the binary (if needed), starts the Server, then connects.

The Server exposes a Unix domain socket on the remote. Each client tunnels to it through a separate SSH control connection or by multiplexing the user's existing SSH session (the exact transport will be decided at implementation time — SSH ControlMaster multiplexing or a dedicated forwarding session are both options).

## Configuration Interface

`~/.sshx/config.yaml`:

```yaml
strict: false
features:
  commandBridge: true
  ports:
    auto: true
    # Only forwards 127.0.0.1 / ::1 ports by default
    # bindAll: false   # future option to enable 0.0.0.0 ports
  domains:
    enabled: true
    suffix: xiaot.sshx   # {user}.sshx

commands:
  deny: []
```

SSH-compatible invocations should not be broken by sshx-specific flags. Prefer `--no-wrap` or `SSHX_DISABLE=1`:

```
sshx --no-wrap remote
SSHX_DISABLE=1 sshx remote
```

`--no-wrap` takes precedence over `SSHX_DISABLE` (always unwraps regardless of environment variable).

## Test Plan

### Compatibility

- `sshx nonmatched ...` behaves identically to `ssh nonmatched ...`.
- Verify verbatim passthrough of: `-V`, `-G`, `-Q`, `-F`, `-o`, `-J`, `-L`, `-R`, `-D`, `-N`, `-T`, `user@host`, and remote commands.
- Verify that `alias ssh=sshx` does not break normal SSH workflows.
- `sshx local` on the client gives a clear reserved-name error and never falls back to SSH.
- `sshx --no-wrap` bypasses all wrapping and is equivalent to raw `ssh`.

### Command Bridge

- `sshx remote uname -a` output and exit code match `ssh remote uname -a`.
- Inside a wrapped remote session, `sshx local uname -a` returns the client machine's `uname -a`.
- Running `sshx local ...` without an active Server session should report a clear error.
- stdout, stderr, and exit code propagate correctly.
- stdin (piped, non-interactive) propagates correctly in batch mode.

### Server Lifecycle

- The first matched connection installs the remote binary and starts the shared Server.
- A second concurrent client connects to the same Server (shared port forwarding, domain routing).
- The Server stays alive through brief disconnects (idle timeout).
- On Server exception, fall back to ordinary SSH (unless `strict: true`).

### Feature Integration

- Remote port sniffing creates forwardings and domain routes (localhost ports only).
- Concurrent terminals to the same host share forwarding state — no port conflicts.
- DNS suffix setup requires authorization only once; subsequent dynamic domain names work without repeated prompts.
- The Server binary discovers the client via `SSHX_BRIDGE_SOCKET` injected into the Server process environment.

## Assumptions

- `local` is a globally reserved target name across the entire sshx binary.
- OpenSSH remains the authoritative SSH implementation.
- Unmatched invocations are pure passthrough.
- v1 target platforms: macOS/Linux clients, Linux servers.
- Remote wrapper is installed per-user under `~/.sshx`.
- Remote-to-local commands require an active Server session with a connected client on the remote.
- v1 development: manual binary installation; v2: distribution via GitHub releases.
- v1 command bridge supports non-interactive commands only (no PTY allocated for bridge commands).
