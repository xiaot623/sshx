# sshx

> 透明 SSH 增强 — 为你的 SSH 工作流添加远程到本地命令执行、自动端口转发和本地域名访问。无需时零副作用。

**sshx** 是 OpenSSH 的即插即用封装器。设置 `alias ssh=sshx` 后，你现有的 SSH 工作流完全不受影响——所有参数、配置和连接都原样透传。但当连接到启用了 sshx 特性的主机时，你会获得一个持久化的共享远程服务器，提供以下能力：

- 🔄 **反向命令桥** — 在*远程*执行 `sshx local <cmd>`，命令实际在你的本地机器上运行，stdout、stderr、退出码和 stdin 全部正确传递。
- 🔌 **自动端口转发** — 远程回环监听端口（如在 `localhost:8080` 上的开发服务器）被自动检测并转发到本地。
- 🌐 **本地域名绑定** — 在本地浏览器中通过 `<主机>.<用户名>.sshx:<端口>` 访问转发端口，无需手动设置 `-L` 参数。

## 为什么选择 sshx？

| 没有 sshx | 有 sshx |
|---|---|
| `ssh remote` — 正常工作 | `ssh remote` — 正常工作，*同时*在后台启动服务器 |
| 需要在会话中从本地复制文件到远程 | `sshx local cat ~/file.txt` — 在本地执行，输出流式传回远程 |
| `localhost:3000` 上的开发服务器无法访问 | 执行 `sshx debian` 后，在本地浏览器打开 `http://debian.<你的用户名>.sshx:3000` |
| 多个终端各自需要设置 `-L` 转发 | 一个共享守护进程，一次转发，所有终端受益 |
| 连接前忘记设置端口转发 | 端口自动检测并转发 |

sshx 设计为**可安全别名化**。未配置 sshx 的主机完全不受影响——不启动守护进程，不创建文件，零性能开销。

---

## 特性

### 📦 即插即用兼容性

- `sshx [任意 ssh 参数...]` 委托给真正的 OpenSSH。
- 所有 SSH 参数透传：`-F`、`-o`、`-J`、`-L`、`-R`、`-D`、`-N`、`-T`、`-V`、`-G`、`-Q` 等。
- `~/.ssh/config` 解析由 OpenSSH 处理——不做重复实现。
- 随时可通过 `sshx --no-wrap` 或 `SSHX_DISABLE=1` 跳过所有 sshx 行为。

### 🔄 远程到本地命令桥

在 SSH 会话中，从远程执行**本地机器**上的命令：

```sh
# 在远程的 sshx 封装会话中：
sshx local cat ~/my-local-file.txt
sshx local open -a "Google Chrome" "http://localhost:3000"
sshx local pbcopy < /tmp/some-data
```

- stdout、stderr 和退出码正确传递。
- stdin 以批量模式发送——管道输入数据会到达本地命令。
- 策略：通过可配置的拒绝列表控制哪些命令被阻止。

### 🔌 自动端口检测与转发

当远程有进程在 `127.0.0.1` 上监听（如 `npm run dev` 在端口 3000），sshx 会检测到并：

1. 向本地守护进程广播该端口。
2. 通过 SSH 创建共享 TCP 转发。
3. 绑定 SSH 目标域名，例如 `debian.<用户名>.sshx`。

sshx 会优先使用与远端监听相同的本地端口。如果该本地端口已被占用，则自动尝试下一个端口（`+1`），直到找到可用端口。可通过 `sshx forward` 查看当前映射。

### 🌐 本地域名访问（macOS、Linux）

- 本地 DNS 应答器在 `127.0.0.1:53` 动态解析 `*.sshx` 域名。
- 域名解析到 localhost；URL 里的端口选择对应的本地转发监听。
- macOS 上，`/etc/resolver/<suffix>` 仅配置一次（需要时通过 `sudo` 授权）。
- 同一主机上的所有终端共享一个 DNS 应答器和转发守护进程。

### 🏗️ 共享服务器架构

- 每个 client 的 target alias 对应**一个共享服务器守护进程**，在远程安装到 `~/.sshx_server/<uuid>`，并由该 client 的并发 SSH 会话共用。
- 客户端通过隐藏的 `socket-proxy` SSH 通道连接。
- 服务器集中管理端口嗅探、转发状态和命令桥路由。
- 服务器在客户端短暂断开期间保持运行，在所有客户端断开后的空闲超时后退出。

---

## 安装

### 通过 npm 安装

```sh
npm install -g @hahahhh/sshx@next
```

### 从源码构建

```sh
git clone https://github.com/xiaot623/sshx.git
cd sshx
go build -o ./bin/sshx ./cmd/sshx
```

将二进制文件复制到 `$PATH` 中的位置：

```sh
cp ./bin/sshx /usr/local/bin/sshx
```

### Shell 别名（推荐）

添加到 `~/.bashrc` 或 `~/.zshrc`：

```sh
alias ssh=sshx
```

别名是安全的——不匹配的主机零开销、零副作用。

---

## 快速开始

### 1. 创建配置文件

```sh
mkdir -p ~/.sshx
```

`~/.sshx/config.yaml`：

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

### 2. 正常连接

```sh
sshx my-server
sshx my-server uname -s
sshx -p 2222 user@my-server hostname
```

所有已有的 SSH 选项均可使用——`-F`、`-o`、`-J`、`ProxyJump` 等由 OpenSSH 处理。

### 3. 尝试命令桥

在远程的 SSH 会话中：

```sh
sshx local uname -s
# → Darwin（你本地机器的操作系统）
```

### 4. 在远程启动开发服务器

在远程启动一个监听 localhost 的服务器：

```sh
python3 -m http.server 8080 --bind 127.0.0.1
```

在**本地**机器上打开：

```
http://my-server.<你的用户名>.sshx:8080
```

无需 `-L` 参数，无需手动转发。

如果本地端口 `8080` 已被占用，sshx 会继续尝试 `8081`、`8082` 等端口。可用以下命令查看实际端口：

```sh
sshx forward
# 8080 -> my-server:8080
```

---

## 配置参考

`~/.sshx/config.yaml`：

```yaml
# 严格模式：如果 sshx 服务器失败，拒绝连接而不是回退到普通 SSH。
# 默认：false（优雅回退）。
strict: false

features:
  # 远程到本地命令桥（在远程执行 sshx local <cmd>）
  commandBridge: true

  ports:
    # 自动检测远程回环 TCP 监听端口并转发。
    auto: true
    # 未来：同时检测 0.0.0.0 监听。
    # bindAll: false

  domains:
    # 启用本地域名绑定（<host>.<user>.sshx:<port>）。
    enabled: true
    # 自定义域名后缀。默认：<本地用户名>.sshx
    suffix: user.sshx

commands:
  # 阻止通过命令桥执行的命令列表。
  deny: []
```

---

## 工作原理

```
┌─────────────────────────────────┐     ┌─────────────────────────────────┐
│  你的本地机器                     │     │  远程主机                        │
│                                  │     │                                 │
│  终端 A ── SSH ─────────────────┼─────┼── sshx 服务器（共享守护进程）     │
│  终端 B ── SSH ─────────────────┼─────┼──   │                          │
│  终端 C ── SSH ─────────────────┼─────┼──   ├── 端口嗅探                 │
│                                  │     │     ├── 端口转发                 │
│  sshx 本地守护进程                │     │     ├── 命令桥路由               │
│    ├─ 端口代理（共享）            │     │     └── Socket-proxy 端点       │
│    └─ DNS 应答器 (127.0.0.1)    │     │                                 │
└─────────────────────────────────┘     └─────────────────────────────────┘
```

1. **连接**：`sshx remote` 打开一个正常的 SSH 会话，并在远程 `~/.sshx_server/<uuid>` 下启动（或连接到）该 client-target 对应的 `sshx 服务器`。
2. **桥接通道**：一条隐藏的 `socket-proxy` SSH 通道将本地守护进程连接到远程服务器。
3. **端口嗅探**：服务器读取 `/proc/net/tcp*`（Linux）来检测回环监听端口。
4. **转发**：检测到的端口通过单个共享本地守护进程使用 `ssh -W` 转发。
5. **域名**：本地 DNS 应答器将 `<target>.<suffix>` 映射到 localhost。浏览器 URL 中的端口选择对应的本地转发端口。

当 `sshx` 用于**不匹配的主机**时（无 sshx 配置或主机不在范围内），直接 `exec` 真正的 `ssh`——无守护进程，无安装，无开销。

---

## 安全与绕过

- `sshx --no-wrap ...` — 跳过所有 sshx 行为，直接调用原始 `ssh`。
- `SSHX_DISABLE=1 sshx ...` — 与 `--no-wrap` 相同，适用于脚本场景。
- 在**客户端**上执行 `sshx local ...`（非远程会话中）— 立即报错并给出清晰提示。`local` 是全局保留名称。
- 不匹配的主机纯透传——不创建文件，不启动进程。

---

## 平台支持

| 平台 | 客户端 | 服务器 |
|---|---|---|
| macOS | ✅ | — |
| Linux | ✅ | ✅ |
| Windows | 🔜 | — |

- **客户端**：macOS 和 Linux 完全支持。
- **服务器**：远程 sshx 服务器需要 Linux（使用 `/proc/net/tcp*` 进行端口检测）。

---

## 项目结构

```
sshx/
├── cmd/sshx/          # 主入口
├── internal/
│   ├── cli/           # CLI 解析、主机检测
│   ├── sshcompat/     # SSH 参数兼容
│   ├── config/        # YAML 配置
│   ├── protocol/      # 客户端-服务器通信协议
│   ├── bridge/        # 命令桥（远程 → 本地执行）
│   ├── ports/         # 端口嗅探（/proc/net/tcp*）
│   ├── forward/       # TCP 转发
│   ├── domain/        # DNS 解析器
│   └── locald/        # 本地守护进程（socket、DNS、转发）
├── scripts/           # 集成测试
├── go.mod
├── go.sum
└── README.md
```

---

## 路线图

- [x] **v1** — 命令桥（非交互式）、自动端口转发、域名绑定、共享服务器
- [ ] **v2** — 命令桥流式 stdin、远程 FUSE 挂载、GitHub 二进制发布、Windows 客户端支持

---

## 许可证

MIT
