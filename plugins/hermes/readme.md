# Golem Hermes 插件部署手册

本文档给出从零部署 Golem Hermes 插件、Hermes Agent、Hermes Gateway relay，以及完成微信全链路验收的逐步操作。命令和配置依据以下实现与官方资料核对：

- [Hermes Agent 官方安装文档](https://hermesagent.org.cn/docs/getting-started/installation)
- [Hermes 消息网关文档](https://hermesagent.org.cn/docs/user-guide/messaging)
- [NousResearch/hermes-agent](https://github.com/NousResearch/hermes-agent)
- [Relay Connector Contract v1](https://github.com/NousResearch/hermes-agent/blob/main/docs/relay-connector-contract.md)
- 本仓库的 Golem Host 插件发现、配置和 `/pm` 管理实现

> Hermes 官方 relay contract v1 目前仍标记为 experimental。部署前建议固定 Hermes Agent 版本，并在升级 Hermes 后重新完成本文的验收步骤。

## 1. 部署后的结构

完整消息链路如下：

```text
微信消息
  -> Golem Host
  -> Hermes Go Adapter
  -> Durable Inbox (SQLite)
  -> Reorder / Routing
  -> Control | Interactive | Job workers
  -> Hermes Gateway relay
  -> Hermes Agent / Model
  -> output proposal
  -> Transactional Outbox
  -> Golem message.Ability
  -> 微信
```

职责边界：

- Golem Hermes 插件监听微信事件、持久化输入、控制调度、下载微信媒体并可靠发送最终回复。
- Hermes Gateway 主动连接 Golem 暴露的 WebSocket connector。
- Hermes Agent 负责推理，但不能直接持有微信 Receiver，也不能绕过 Go Outbox 发送微信。
- SQLite 是单节点事实源。已进入 Inbox 的消息以及已提交 Outbox 的回复可以在进程重启后恢复。

当前官方 relay v1 的标准 outbound action 只覆盖文本类 `send`，尚未发布图片/表情 outbound action。插件 `0.5.0` 增加了受控的本地 Capability API 与 Hermes 用户插件：Agent 可以自主搜索并选择表情，但不能指定 wxId、聊天目标、媒体 URL 或本地路径。选择结果先绑定当前 Run，再与最终文字一起原子提交到 SQLite Outbox，最后由 Golem 以 `message.TypeEmoji` 发送。这个扩展没有绕过官方 Relay 的文本交付，也没有把微信发送权交给 Hermes。

推荐把 Golem Host 和 Hermes Gateway 部署在同一台机器、同一个操作系统文件系统中。文本可以跨主机工作，但 relay v1 传递输入图片时使用本地绝对文件路径；跨主机必须把媒体目录以完全相同的绝对路径挂载到两端。

## 2. 前置条件

### 2.1 Golem 侧

- Go 1.26 或更高版本。
- Git。
- 一个能够正常登录微信、接收消息和发送消息的 Golem Host。
- Hermes connector 默认端口 `8789` 未被占用。
- 生产环境必须配置 Golem Owner。`/pm` 管理命令和 `/hermes` Hermes 命令都依赖该 wxId；插件在 Owner 为空时会拒绝所有 `/hermes` 命令。

检查：

```bash
go version
git --version
```

### 2.2 Hermes Agent 侧

- 能访问一个上下文窗口至少为 64K 的模型 Provider。Hermes 官方会拒绝上下文过小的模型。
- 同机部署时无需开放公网端口。
- 跨主机部署时需要 WSS/TLS 反向代理。HMAC 只提供身份认证，不加密消息内容。

## 3. 获取代码

如果尚未克隆 Golem：

```bash
git clone https://gitee.com/fCheng715/golem.git
cd golem
git checkout codex/hermes-sticker-capabilities
```

确认插件目录存在：

```bash
git log -1 --oneline
ls plugins/hermes
```

Windows PowerShell 可使用：

```powershell
git log -1 --oneline
Get-ChildItem .\plugins\hermes
```

## 4. 构建 Hermes 插件

Golem Host 会从其当前工作目录下的 `plugins/` 递归寻找插件。可执行文件名必须以 `golem_plugin_` 开头；Hermes 推荐使用：

- Windows：`golem_plugin_hermes.exe`
- Linux/macOS：`golem_plugin_hermes`

### 4.1 Windows 构建

从 Golem 仓库根目录运行：

```powershell
New-Item -ItemType Directory -Force .\host\plugins | Out-Null
Set-Location .\plugins\hermes

# 如果下载依赖需要本机代理，取消下面两行的注释；只对当前 PowerShell 生效
# $env:HTTP_PROXY = 'http://127.0.0.1:7897'
# $env:HTTPS_PROXY = 'http://127.0.0.1:7897'

go mod download
go test ./...
go build -trimpath -ldflags '-s -w' -o ..\..\host\plugins\golem_plugin_hermes.exe .
Set-Location ..\..
```

### 4.2 Linux/macOS 构建

```bash
mkdir -p host/plugins
cd plugins/hermes

# 仅在确实需要代理时设置
# export HTTP_PROXY=http://127.0.0.1:7897
# export HTTPS_PROXY=http://127.0.0.1:7897

go mod download
go test ./...
go build -trimpath -ldflags '-s -w' -o ../../host/plugins/golem_plugin_hermes .
chmod +x ../../host/plugins/golem_plugin_hermes
cd ../..
```

### 4.3 验证构建产物

Windows：

```powershell
Get-Item .\host\plugins\golem_plugin_hermes.exe
```

Linux/macOS：

```bash
ls -l host/plugins/golem_plugin_hermes
```

> 仓库当前 `plugins/Taskfile.yml` 的批量插件列表尚未包含 `hermes`，因此本手册使用上面的直接 `go build` 命令，不要依赖 `task build:hermes`。

## 5. 确定 Golem Host 工作目录

Host 使用相对路径 `plugins/` 和 `data/`。必须从包含这些目录的部署目录启动。例如：

```text
golem/host/
  golem.exe                 # 或 golem
  plugins/
    config.toml
    golem_plugin_hermes.exe # Linux 无 .exe
  data/
    config.toml
```

推荐进入 `host` 后启动：

```powershell
Set-Location C:\path\to\golem\host
.\golem.exe
```

```bash
cd /path/to/golem/host
./golem
```

如果从仓库根目录运行 Host，则插件必须放在仓库根目录的 `plugins/`，而不是 `host/plugins/`。部署时只选择一种固定工作目录，避免 Host 加载了错误位置的配置或旧二进制。

## 6. 配置 Golem Owner

Owner 才应具有 `/pm` 管理权限。使用 lib 模式 Host 时，在 Host 工作目录的 `data/config.toml` 中设置：

```toml
owner = "wxid_your_owner"
forbidden = "无权限执行此操作"
```

不要在未配置 Owner 的生产机器人上使用 `/pm info hermes`，该命令会显示插件配置，其中可能包含 `relay_shared_secret`。

## 7. 配置 Hermes 插件

配置文件位于 Host 工作目录下：

```text
plugins/config.toml
```

如果 `[hermes]` 不存在，Host 第一次发现插件时会写入插件默认配置。生产部署建议在首次启动前显式加入下面的配置。

### 7.1 最小本机配置

Golem 和 Hermes Agent 位于同一台机器、同一操作系统时，可先使用无鉴权 loopback 联调：

```toml
[hermes]
enable = true
mode = "blacklist"
limits = []

[hermes.config]
data_dir = "data/hermes"
shutdown_grace_seconds = 15
bot_names = ["hermes"]

[hermes.config.ingress]
reorder_window_milliseconds = 120
durable_accept_timeout_milliseconds = 100

[hermes.config.routing]
social_mode = "agent"
sample_rate = 1.0
decision_timeout_milliseconds = 800
ordinary_freshness_seconds = 5

[hermes.config.scheduler]
router_workers = 2
interactive_workers = 4
interactive_reserved_workers = 1
job_workers = 2
tool_workers = 8
max_active_sessions = 512

[hermes.config.agent]
mode = "relay"
relay_listen = "127.0.0.1:8789"
relay_path = "/relay"
silence_rules_file = "/opt/software/wechat/data/hermes/workspace/silence-rules.txt"
async_delivery_enabled = true

[hermes.config.output]
delivery_semantics = "at_least_once"
workers = 4
max_attempts = 12
ambiguous_max_attempts = 2
send_timeout_seconds = 15
retry_min_seconds = 1
retry_max_seconds = 300
send_interval_milliseconds = 700
send_jitter_milliseconds = 250
```

说明：

- `social_mode = "agent"` 表示所有群消息都先持久化，再交给 Hermes 结合共享群聊上下文与自身兴趣决定是否自然参与；@/引用只作为 `group addressed` 信号，不是硬门槛。
- `relay_path` 应保持 `/relay`。Hermes 官方 Gateway 会把 base URL 规范化到 `/relay`。
- `data_dir` 相对于 Host 工作目录。生产环境推荐改为双方都能访问的绝对路径。
- `delivery_semantics` 当前只支持 `at_least_once`。发送结果不确定时可能重复，但不会静默丢弃已提交回复。
- `async_delivery_enabled` 默认关闭。开启后，Hermes `delegate_task(background=true)` 的完成结果通过稳定票据进入同一 Transactional Outbox；普通无 Run Relay send 仍然拒绝。

首次加载插件前创建静默规则文件：

```bash
install -d -m 700 /opt/software/wechat/data/hermes/workspace
cat > /opt/software/wechat/data/hermes/workspace/silence-rules.txt <<'EOF'
# Provider 渲染出的静默说明；一行一条规则
exact:[silence]
prefix:I don't need to respond to this
EOF
chmod 600 /opt/software/wechat/data/hermes/workspace/silence-rules.txt
```

规则支持 `exact:`、`prefix:`、`suffix:`，忽略空行和 `#` 注释，匹配不区分大小写。Connector 在每次最终回复时重新读取文件，写入后下一条消息即生效，无需 reload。文件必须小于等于 64 KiB 且不超过 256 条规则；插件首次加载时会拒绝不存在或格式错误的文件，运行中读取失败会明确记录 `silence rules reload failed`，不会把 Run 留在 `running`。

### 7.2 推荐的 HMAC 配置

即使是本机，也可以启用官方 HMAC upgrade token。先生成 32 字节随机 secret。

Windows PowerShell：

```powershell
$bytes = New-Object byte[] 32
$rng = [Security.Cryptography.RandomNumberGenerator]::Create()
$rng.GetBytes($bytes)
$rng.Dispose()
-join ($bytes | ForEach-Object { $_.ToString('x2') })
```

Linux/macOS：

```bash
openssl rand -hex 32
```

把结果保存到密码管理器，然后用下面内容替换最小配置中的 `[hermes.config.agent]` 段；不要在同一个 TOML 文件中重复声明该段：

```toml
[hermes.config.agent]
mode = "relay"
relay_listen = "127.0.0.1:8789"
relay_path = "/relay"
relay_gateway_id = "golem-hermes"
relay_shared_secret = "替换为刚生成的随机值"
```

`relay_gateway_id` 与 `relay_shared_secret` 必须同时配置，否则插件拒绝启动。无鉴权 relay 只能监听 loopback；代码会拒绝 `0.0.0.0:8789` 之类的无鉴权监听。

### 7.3 跨主机配置

跨主机时不要直接把明文 `ws://` 暴露到公网。推荐：

1. Golem connector 仍绑定 loopback 或受防火墙保护的内部地址。
2. 使用 Caddy、Nginx 或其他反向代理提供 `wss://connector.example.com/relay`。
3. 同时启用 HMAC。
4. 让 Hermes 的 `GATEWAY_RELAY_URL` 使用 `https://connector.example.com`。
5. 如果需要处理输入图片，把 Golem 的媒体目录挂载到 Hermes 主机完全相同的绝对路径。
6. 如果启用表情能力，反向代理还必须把 `/capabilities/v1/stickers/*` 转发到同一个 Golem listener，并让 `GOLEM_CAPABILITIES_URL=https://connector.example.com`；不要用明文 HTTP 传输 Capability token。

HMAC 防止未授权 Gateway 接入，但不替代 TLS。

### 7.4 配置项说明

| 配置 | 默认值 | 作用 |
| --- | --- | --- |
| `data_dir` | `data/hermes` | SQLite 和媒体对象目录 |
| `shutdown_grace_seconds` | `15` | 插件卸载时等待 worker 和 outbox 收敛的时间 |
| `bot_names` | `["hermes"]` | 群聊中识别 @/称呼的别名 |
| `ingress.reorder_window_milliseconds` | `120` | 普通消息短窗口重排时间 |
| `ingress.durable_accept_timeout_milliseconds` | `100` | OnEvent 等待 Inbox 持久化的上限 |
| `routing.social_mode` | `agent` | `agent` 让 Hermes 自主参与；`rules` 仅处理私聊/@/引用；`observe` 只观察普通群聊；`hybrid` 预留给独立 SocialDecider，未注入时安全降级观察 |
| `routing.sample_rate` | `1.0` | SocialDecider 的确定性采样率 |
| `scheduler.interactive_workers` | `4` | 短对话 worker 数量 |
| `scheduler.job_workers` | `2` | 长任务 worker 数量 |
| `scheduler.tool_workers` | `8` | Go Capability Broker 全局并发上限 |
| `agent.mode` | `relay` | 推荐 `relay`；`http` 仅作兼容模式 |
| `agent.relay_listen` | `127.0.0.1:8789` | Golem connector 监听地址 |
| `agent.relay_path` | `/relay` | 官方 Gateway WebSocket 路径 |
| `agent.silence_rules_file` | 空 | 可热更新的群聊静默回复规则文件；仅支持 `exact:`、`prefix:`、`suffix:` |
| `agent.async_delivery_enabled` | `false` | 为 Hermes `0.18.2` 后台子代理启用稳定票据与 Transactional Outbox 回流 |
| `agent.timeout_seconds` | `120` | 仅用于 `http` 兼容模式的墙钟超时；`relay` 模式忽略该值，由 Hermes `agent.gateway_timeout` 管理活性 |
| `output.workers` | `4` | Outbox Dispatcher 数量；不同 session 可并行 |
| `output.max_attempts` | `12` | 超过后进入 dead letter |
| `output.ambiguous_max_attempts` | `2` | 回执不确定时最多尝试次数；等待和执行后台重试均不阻塞同会话后续消息 |
| `output.send_timeout_seconds` | `15` | 单次微信发送超时 |
| `output.retry_min_seconds` | `1` | 重试退避下限 |
| `output.retry_max_seconds` | `300` | 重试退避上限 |

以下字段属于前向兼容/预留调度项，0.5.x 不应作为严格容量承诺：`router_workers`、`interactive_reserved_workers`、`max_active_sessions`、`ordinary_freshness_seconds`、`send_interval_milliseconds`。`send_jitter_milliseconds` 当前用于 Outbox 重试抖动。

`agent.base_url`、`api_key`、`model`、`system_prompt` 只用于 `agent.mode = "http"` 的兼容模式。relay 模式的模型和系统提示由 Hermes Agent 自己配置。

### 7.5 可选：配置 Agent 自主表情回复

该能力默认关闭。开启后，Agent 每一轮都可以自行选择四种结果：不回复、仅文字、仅表情、文字加表情。插件不设置固定概率，也不会按关键词强制发送；实际倾向由 Hermes 的人格、系统提示和当前上下文决定。

先生成专用控制面令牌。不要复用模型 API Key 或 Relay HMAC secret：

```bash
openssl rand -hex 32
```

先创建仅供 Hermes 插件读取的凭据文件，例如 Host 工作目录下的 `data/hermes/capabilities.env`：

```dotenv
GOLEM_CAPABILITIES_TOKEN=replace-with-the-generated-secret
APIHZ_ID=replace-with-apihz-id
APIHZ_KEY=replace-with-apihz-key
```

```bash
chmod 600 data/hermes/capabilities.env
```

该文件由 Hermes 插件进程在加载时读取，不会注入 Golem Host 环境，也不需要修改 systemd、停止或重启 Host。设置文件权限后，在现有 `plugins/config.toml` 中只引用文件路径。不要重复声明已经存在的 `[hermes.config]` 或 `[hermes.config.agent]`：

```toml
[hermes.config.capabilities]
environment_file = "data/hermes/capabilities.env"

[hermes.config.capabilities.sticker]
enabled = true
default_provider = "apihz"
max_candidates = 5
candidate_ttl_seconds = 300
max_media_bytes = 4194304
materialized_cache_max_bytes = 67108864

[[hermes.config.capabilities.sticker.providers]]
id = "apihz"
driver = "http_json"
endpoint = "https://cn.apihz.cn/api/img/apihzbqb.php"
method = "GET"
timeout_seconds = 10
requests_per_minute = 8
max_query_runes = 10
allowed_media_hosts = ["res.apihz.cn"]

[hermes.config.capabilities.sticker.providers.query]
id = "${env:APIHZ_ID}"
key = "${env:APIHZ_KEY}"
type = "2"
limit = "${limit}"
words = "${query}"
page = "${page}"

[hermes.config.capabilities.sticker.providers.response]
success_path = "code"
success_values = ["200"]
items_path = "res"
error_path = "msg"

[hermes.config.capabilities.sticker.providers.response.url]
transforms = ["trim", "markdown_link_target"]
```

`environment_file` 由 Hermes 插件在 `/pm load hermes` 时读取，并为 Capability token 和 `${env:NAME}` Provider 模板提供值；插件文件中的值优先于同名 Host 环境变量。Host 环境变量来源仅为旧部署兼容，新部署不需要修改 systemd、重启或停止 Golem Host。`/pm info hermes` 只显示凭据文件路径，不显示文件内容。

这里必须使用 `type = "2"`，它表示关键词搜索；`type = "1"` 是随机图片，不符合 Agent 根据语义选择候选的流程。APiHz 的 `words` 最多 10 个中文字符，Bridge 会按 Unicode 字符而不是 UTF-8 字节安全限制长度。`requests_per_minute = 8` 给服务商约 10 次/分钟的注册用户限制留出余量。

`materialized_cache_max_bytes` 是所有短期已下载候选共享的内存预算，必须不小于单图 `max_media_bytes`。默认 64 MiB；空间不足时会淘汰完整候选 ID，而不会按旧 ID 重新下载可能已经变化的图片。

Hermes 出站图片和表情不会调用 SDK 的 `cdn.UploadImage`。Outbox 保存完整媒体字节，Dispatcher 通过 `message.Send` 的 `Media.Data` 直接交给 Host，再由 Host 的消息接口完成 CDN 上传和微信发送；不要改成插件先上传、再把 `file_id/key` 交给 Host 的两段式链路。

凭据只能写成 `${env:NAME}`。Golem 在加载时会输出插件配置，代码会拒绝 `key`、`token`、`authorization` 等敏感字段中的明文值，也会拒绝把这类参数直接放在 endpoint URL 中。API 返回 HTTP 200 仍不代表业务成功；上面的映射还会检查 `code == 200`，然后从 `res[]` 提取 URL。

更换服务商时不需要改 Hermes 用户插件，只需新增或替换 `[[...providers]]`：

- GET 参数放在 `query`，POST 表单放在 `form`；POST 也可以同时配置二者。
- 模板支持 `${query}`、`${limit}`、`${page}`、`${env:NAME}`。
- JSON 路径使用 GJSON；`items_path` 可指向标量、字符串数组或对象数组。
- `response.url.path` 和 `response.description.path` 是相对于每个候选项的路径；字符串数组的 URL path 留空。
- 字段转换支持 `trim`、`markdown_link_target`、`html_unescape`。
- `allowed_media_hosts` 必须列出实际图片 CDN；下载器还会拒绝私网 IP、非 HTTPS、越权重定向、超限文件和伪造图片。
- 多个 Provider 同时启用时，`default_provider` 决定当前 Agent 工具使用哪一个；Provider ID 不暴露微信权限。

## 8. 首次启动 Golem 并验证插件

从 Host 工作目录启动 Golem。启动日志应包含：

```text
插件加载成功 name=hermes ... version=0.7.4
[hermes] 内核存储已启动 database=.../hermes.db
```

检查 connector 端口。

Windows：

```powershell
Get-NetTCPConnection -State Listen -LocalPort 8789
Test-NetConnection 127.0.0.1 -Port 8789
```

Linux：

```bash
ss -ltnp | grep 8789
```

此时没有 Hermes Gateway 连接是正常的。connector 正在等待 Gateway 主动拨入。

## 9. 安装 Hermes Agent

建议 Hermes Agent 与 Golem 使用相同操作系统：

- Golem 在 Windows 原生运行：安装 Windows 原生 Hermes。
- Golem 在 Linux 运行：在同一 Linux 主机安装 Hermes。
- 不推荐 Windows Golem 配合 WSL Hermes 处理图片，因为 Windows 与 WSL 的绝对路径格式不同。

### 9.1 Windows 原生安装

在新的 PowerShell 窗口执行官方安装命令：

```powershell
iex (irm https://hermes-agent.nousresearch.com/install.ps1)
```

若官方 CDN 安装脚本不可用，可使用官方仓库脚本：

```powershell
iex (irm https://raw.githubusercontent.com/NousResearch/hermes-agent/main/scripts/install.ps1)
```

安装完成后关闭并重新打开 PowerShell，然后验证：

```powershell
hermes --version
hermes doctor
```

Windows 默认数据目录：

```text
%LOCALAPPDATA%\hermes\
  .env
  config.yaml
  logs\
  sessions\
  hermes-agent\
```

### 9.2 Linux/macOS 安装

```bash
curl -fsSL https://hermes-agent.nousresearch.com/install.sh | bash
source ~/.bashrc

hermes --version
hermes doctor
```

默认位置：

```text
~/.hermes/hermes-agent/  # 程序
~/.local/bin/hermes      # 命令
~/.hermes/.env           # secret
~/.hermes/config.yaml    # 非敏感配置
```

无桌面浏览器需求的服务器可以跳过浏览器组件：

```bash
curl -fsSL https://hermes-agent.nousresearch.com/install.sh | bash -s -- --skip-browser
```

### 9.3 WSL2

在 WSL 内按 Linux 方式安装。Gateway 首次联调使用前台命令：

```bash
hermes gateway run
```

官方建议 WSL 不依赖 `hermes gateway start`；长期运行可使用：

```bash
tmux new -s hermes 'hermes gateway run'
```

## 10. 配置 Hermes 模型 Provider

先让 Hermes CLI 本身能够正常回答，再接入 Gateway：

```bash
hermes model
```

可选择 Nous Portal、OpenRouter、Anthropic、OpenAI Codex、GLM、Qwen、DeepSeek、Gemini、Ollama 或自定义 OpenAI-compatible endpoint。根据向导完成 Provider 和模型设置。

验证：

```bash
hermes
```

输入一个简单问题，确认得到正常回复后退出。若 CLI 自身不能回复，不要继续排查 relay；先运行：

```bash
hermes doctor
hermes model
```

Hermes 将 secret 写入 `.env`，普通设置写入 `config.yaml`。不要把这两个文件提交到 Git。

## 11. 配置 Hermes relay

### 11.1 配置 `.env`

同机无鉴权的最小配置：

```dotenv
GATEWAY_RELAY_URL=http://127.0.0.1:8789
RELAY_HOME_CHANNEL=disabled
```

启用 HMAC 时使用：

```dotenv
GATEWAY_RELAY_URL=http://127.0.0.1:8789
GATEWAY_RELAY_ID=golem-hermes
GATEWAY_RELAY_SECRET=replace-with-the-same-secret
RELAY_HOME_CHANNEL=disabled
```

如果启用了 7.5 的表情能力，再把下面三项加入同一个 Hermes `.env`。`GOLEM_CAPABILITIES_TOKEN` 必须与 Golem 插件 `environment_file` 中的值完全相同。Hermes 不需要 `APIHZ_ID/APIHZ_KEY`：

```dotenv
GOLEM_CAPABILITIES_URL=http://127.0.0.1:8789
GOLEM_CAPABILITIES_TOKEN=replace-with-the-same-capability-secret
GOLEM_CAPABILITIES_TIMEOUT_SECONDS=10
GOLEM_ASYNC_DELIVERY_ENABLED=true
```

文件位置：

- Windows：`%LOCALAPPDATA%\hermes\.env`
- Linux/macOS/WSL：`~/.hermes/.env`

规则：

- `GATEWAY_RELAY_URL` 填 connector 的 base URL。官方 Gateway 会把 `http://` 转为 `ws://`，把 `https://` 转为 `wss://`，并补上 `/relay`。
- `GATEWAY_RELAY_ID` 必须等于插件的 `relay_gateway_id`。
- `GATEWAY_RELAY_SECRET` 必须等于插件的 `relay_shared_secret`。
- `RELAY_HOME_CHANNEL=disabled` 是专用 Relay profile 的提示抑制哨兵。Hermes 只检查该变量是否存在，因此不会再向首轮微信会话发送 `No home channel is set for Relay`；当前 Hermes v0.18.2 不会把该变量装配成 Relay 的真实 HomeChannel。
- 不要在 URL 中携带 secret。
- 修改 `.env` 后必须重启 Hermes Gateway。
- `GOLEM_ASYNC_DELIVERY_ENABLED` 必须与 Golem 的 `agent.async_delivery_enabled` 同时开启；该能力严格锁定 Hermes Agent `0.18.2`，版本不匹配时用户插件会拒绝加载。

### 11.2 配置 `config.yaml`

在 Hermes 的 `config.yaml` 中合并以下配置。不要覆盖已经配置好的 Provider/模型部分。

```yaml
group_sessions_per_user: false

agent:
  gateway_timeout: 1800
  gateway_timeout_warning: 900

platform_toolsets:
  relay:
    - delegation
    - web
    - file
    - skills
    - no_mcp

display:
  tool_progress: off
  busy_input_mode: queue
  busy_ack_enabled: false
  interim_assistant_messages: false
```

原因：

- `group_sessions_per_user: false` 让同一微信群的不同成员共享一个 Hermes 会话，Agent 才能依据完整群聊上下文判断是否参与。该键是 Hermes `config.yaml` 顶层配置，不要放进 `gateway:`。
- Hermes v0.18.2 的通用配置检查器可能把该键报告为未知顶层键，但 Gateway 的实际加载器仍会读取它。可使用下面的命令核验有效值，不要只根据警告移动配置：

```bash
/usr/local/lib/hermes-agent/venv/bin/python - <<'PY'
from gateway.config import load_gateway_config
print("group_sessions_per_user:", load_gateway_config().group_sessions_per_user)
PY
```

- Golem Hermes 插件 `0.7.4` 起，能力注册同时支持 Hermes 合法的群共享 session key 与群成员隔离 session key。群聊 cron 投递不再依赖该配置必须为 `false`；该选项只决定 Hermes 的群聊上下文是否按成员隔离。
- `agent.gateway_timeout` 是 Hermes 的无活动超时，不是任务墙钟总时长；持续产生模型流或工具活动的长任务可以继续运行。设为 `0` 可关闭自动超时。
- `display.busy_input_mode: queue` 保证 Hermes 忙碌时的新输入排队，不会打断当前任务。
- 微信副作用必须由 Go Capability Broker 和 Transactional Outbox 管理。
- `web` 保留搜索能力；`file` 与 `skills` 允许 Owner 要求 Agent 维护静默规则和 `~/.hermes/skills/` 下的 Skill。`file` 继承 Gateway 服务账户的本地文件权限，因此该配置适合专用 Golem Relay profile。
- Gateway 侧不要给 relay 启用 `terminal`。微信发送等平台副作用必须由 Go Capability Broker 和 Transactional Outbox 管理；下一节的 `golem_stickers` 也只能提交受控提案，不能选择微信目标或直接发送消息。
- relay v1 没有独立的 `turn_completed` frame。关闭 progress、busy ack 和 interim messages，可以避免中间消息被当作延迟 proposal 在最终提交时一起发送。

如果安装了自动注册工具的 Hermes 第三方插件，也应对 relay 禁用。不要让 Hermes Gateway 直接拥有微信发送工具。

`no_mcp` 只禁止 MCP server 自动注入；Hermes 插件注册的 toolset 是另一条路径。Hermes v0.18.2 的 `hermes tools` CLI 使用内建平台白名单，尚未把动态 `relay` 平台加入其中，因此 `hermes tools list --platform relay` 会返回 `Unknown platform 'relay'`。这只是 CLI 的提前校验限制；Gateway 运行时仍会把 `Platform.RELAY` 映射到 `platform_toolsets.relay`。

Linux 上可以使用 Hermes 自带的 Python 直接调用同一运行时解析函数，核验 Relay 最终会加载的 toolset：

```bash
HERMES_PROJECT="$(hermes version | sed -n 's/^Project: //p')"
HERMES_PY="${HERMES_PROJECT%/lib/python*/site-packages}/bin/python"
"$HERMES_PY" - <<'PY'
import logging
logging.disable(logging.CRITICAL)
from hermes_cli.config import load_config
from hermes_cli.tools_config import _get_platform_tools
resolved = sorted(_get_platform_tools(load_config(), "relay"))
print("relay toolsets:", resolved or "<none>")
PY
```

未启用表情能力的专用 Golem Relay profile 预期输出 `file`、`no_mcp`、`skills`、`web`；启用后还应包含 `golem_stickers`。如果出现其他不需要的插件 toolset，把相应名称加入 `known_plugin_toolsets.relay`，但不要加入 `platform_toolsets.relay`；前者表示插件已知，后者才表示启用。由于 v0.18.2 的交互式 `hermes tools` 同样不列出 Relay，这一步需要直接编辑 `config.yaml`。保存后重启 Gateway。

Connector descriptor 还会明确告诉模型：当前聊天回复只需产生普通 final assistant text，由 Relay 自动交付；不要搜索或调用 MCP、reply、messaging、send、notification 工具。

从插件 `0.4.2` 开始，Relay Run 不再使用插件侧 `agent.timeout_seconds` 墙钟 Deadline。此前该 Deadline 可能在 Hermes 仍工作时提前放弃本地 Run，并把 `The request failed temporarily. Please try again later.` 写入微信 Outbox。现在 Relay 的活性完全交给 Hermes `agent.gateway_timeout`；Connector 断线进入持久重试，内部执行失败只记录到 SQLite/日志，不再伪装成 Agent 回复发到微信。

在 `social_mode = "agent"` 下，普通群消息标记为 `group ambient`，@/引用标记为 `group addressed`。Hermes 对 ambient 消息想参与时正常回复；不想参与时输出内部观察标记。Addressed 只是要求可见回复的行为提示；如果模型最终仍返回观察标记，Connector 也会将 Run 成功提交但不创建 Outbox，避免单个异常结果阻塞整个群 Session。该模式意味着每条启用范围内的群消息都会发给模型判断，会增加模型调用量，也应纳入群成员隐私告知。

Golem Host 会优先处理以 `/` 开头的命令，因此微信直接发送 `/new`、`/reset`、`/status` 等内容不会作为普通消息事件到达 Hermes。插件实现了 Golem `CommandPlugin`，注册 `/hermes` 主命令，将后面的第一个位置参数恢复为 Hermes 斜杠命令：

| 微信命令 | 发送给 Hermes | 中文说明 |
| --- | --- | --- |
| `/hermes help` | 不发送 | 插件本地返回本命令表 |
| `/hermes status` | `/status` | 查看当前 Hermes 会话、模型和上下文状态 |
| `/hermes new` | `/new` | 创建全新会话；Hermes 默认可能要求确认 |
| `/hermes reset` | `/reset` | `/new` 的别名；Hermes 默认可能要求确认 |
| `/hermes approve` | `/approve` | 单次批准当前确认请求 |
| `/hermes always` | `/always` | 批准当前重置，并关闭以后同类重置确认 |
| `/hermes cancel` | `/cancel` | 取消当前重置确认请求 |
| `/hermes personality technical` | `/personality technical` | 查看或切换 Hermes 人格；名称是可选位置参数 |

也可以发送 `/hermes -h` 或 `/hermes --help` 查看 SDK 生成的中文参数帮助。为避免从微信开放 Hermes 的重启、更新和其他管理面，插件只接受上表白名单；例如 `/hermes restart` 会被拒绝。

所有 `/hermes` 命令执行都只允许 Host 配置中的 Owner 使用。私聊会再次比较发送者 wxId；群聊成员鉴权由 Host 在调用插件 RPC 前根据 `Message.Member` 完成，插件随后把 principal 固定为 Owner、把 receiver 和 session 保持为当前群。Owner 未配置时插件拒绝命令，不会采用模型对“主人”的自然语言判断代替权限校验。

命令不会从 `OnCommand` 直接调用 Relay。它先作为可信命令事件写入 SQLite Inbox，再经过现有 Routing、Interactive Worker 和 Transactional Outbox；只有 Worker 调用 Relay 的最后一步才把它还原为 `/status` 等原生命令。因此它与同一微信私聊或群聊的普通消息共用 Hermes session，`/hermes reset` 后的 `/hermes always` 也会落入同一个 session。

旧版 `hermes:new` 和 `hermes:reset` 纯文本桥接仍保留兼容，但新部署应使用 `/hermes new` 和 `/hermes reset`。`/hermes` 命令在群聊中不要求额外 @ 机器人，但必须由 Owner 发送。

上面的 `display` 设置适合专门服务 Golem relay 的 Gateway。如果同一个 Hermes profile 还承载其他消息平台，请先确认这些全局显示设置对其他平台的影响，再决定是否拆成独立 `HERMES_HOME` profile。

### 11.3 安装并启用 Hermes 表情工具插件

只有启用 7.5 时才执行本节。把仓库中的完整用户插件目录部署到 Hermes Home；不要只复制 `__init__.py`：

```bash
mkdir -p /root/.hermes/plugins/golem_sticker_capabilities
cp -a /path/to/golem/plugins/hermes/hermes_plugin/golem_sticker_capabilities/. \
  /root/.hermes/plugins/golem_sticker_capabilities/
chmod -R go-rwx /root/.hermes/plugins/golem_sticker_capabilities
```

先用 Hermes 自带 Python 执行插件自测：

```bash
HERMES_PROJECT="$(hermes version | sed -n 's/^Project: //p')"
HERMES_PY="${HERMES_PROJECT%/lib/python*/site-packages}/bin/python"
"$HERMES_PY" /root/.hermes/plugins/golem_sticker_capabilities/test_plugin.py
```

预期 10 项测试全部通过。然后在 `/root/.hermes/config.yaml` 中合并插件启用项、辅助视觉模型和 Relay 工具集：

```yaml
plugins:
  enabled:
    - golem-sticker-capabilities

auxiliary:
  vision:
    provider: auto
    model: "<vision-model-id>"
    base_url: "<optional-openai-compatible-url>"
    timeout: 120
    download_timeout: 30

platform_toolsets:
  relay:
    - web
    - file
    - skills
    - no_mcp
    - golem_stickers
```

`known_plugin_toolsets.relay` 不是启用列表，不要把 `golem_stickers` 只写在那里。真正决定 Relay 能否调用它的是 `platform_toolsets.relay`。`hermes tools list --platform relay` 在 Hermes v0.18.2 会因 CLI 静态白名单返回 `Unknown platform 'relay'`，请使用 11.2 的 Python 解析命令核验运行时结果。

主对话模型不需要支持视觉；候选识图只调用 Hermes v0.18.2 的 `auxiliary.vision`。视觉 Provider 凭据沿用 Hermes 的安全配置方式，不要写入 Golem 配置或仓库。

这三个工具的权限被刻意限制：

1. `golem_sticker_search(query, limit)` 只能提交搜索词，返回与当前 Run 绑定的短期随机候选 ID，不返回媒体 URL。
2. `golem_sticker_inspect(candidate_id)` 只能读取 Golem 已验证的候选字节，并通过固定提示调用辅助视觉模型；Agent 看不到 data URL、来源 URL 或路径。
3. `golem_sticker_select(candidate_id)` 只能选择本轮候选。检查和选择复用同一份缓存字节；过期或淘汰 ID 不会重新下载成其他内容。
4. Golem 把 Emoji effect 暂存在当前 Run；Hermes 正常输出 final 文字时形成“文字加表情”，返回内部 token 时形成“仅表情”。
5. Agent 对普通群消息决定保持沉默时，任何暂存表情都会被丢弃；内部 token 永远不会进入微信 Outbox。
6. 最终文字和所有 effect 在同一个 Run 成功事务中写入 Outbox，最终文字位于暂存 effect 之前，effect 保持选择顺序。普通消息发送失败沿用已有至少一次重试和 dead-letter 机制；视频发送结果不确定时直接进入 dead-letter，避免重复发送同一视频。

不需要在插件里配置固定表情概率。可以在当前 Hermes 人格中加入类似下面的倾向说明，由 Agent 结合上下文自行权衡：

```text
表情包只在它比文字更自然地表达反应时偶尔使用。严肃、技术或敏感话题优先使用文字；不要为了展示能力而机械地每轮发送表情。
```

修改人格只影响选择倾向，不会扩大工具权限。完成配置后执行 `/pm reload hermes`，让 Capability API 和 Provider 随插件重新加载；不需要停止或重启 Golem Host。再重启 Hermes Gateway，让用户插件、环境变量和 toolset 生效。

## 12. 前台启动 Gateway 完成首次握手

确保顺序为：

1. Golem Host 已启动。
2. Hermes 插件已加载，端口 `8789` 正在监听。
3. 再启动 Hermes Gateway。

前台运行：

```bash
hermes gateway run
```

成功时应看到 Gateway 注册 relay adapter，并完成 `hello -> descriptor` 握手。descriptor label 为：

```text
Golem WeChat
```

connector contract version 应为 `1`。

如果 Gateway 先启动，生产 transport 会重连，但首次部署仍建议按上述顺序启动，日志更容易判断。

## 13. 把 Gateway 安装为后台服务

先完成前台联调，再安装后台服务。

### 13.1 Windows 原生

```powershell
hermes gateway install
hermes gateway start
hermes gateway status
```

管理命令：

```powershell
hermes gateway restart
hermes gateway stop
hermes gateway uninstall
```

Windows 使用计划任务并在必要时回退到 Startup 目录，不要求管理员权限。

### 13.2 Linux/macOS

```bash
hermes gateway install
hermes gateway start
hermes gateway status
```

若运行在容器或 WSL，优先使用 `hermes gateway run` 作为前台主进程。

## 14. 启用 Hermes 插件

### 14.1 通过配置全局启用

确认 `plugins/config.toml`：

```toml
[hermes]
enable = true
mode = "blacklist"
limits = []
```

Host 启动时会加载所有符合命名规则的插件。`enable` 控制事件是否分发给 Hermes。

### 14.2 通过微信 `/pm` 命令启用

在与机器人的私聊中，以 Owner 身份发送：

```text
/pm list
/pm info hermes
/pm enable hermes
```

注意：

- 在私聊执行 `/pm enable hermes` 是全局启用。
- 在群聊执行时只启用当前群聊。
- `/pm info hermes` 可能显示 relay secret，只允许 Owner 使用。
- Host 当前在加载插件时总会调用 `OnLoad`；即使 `enable = false`，connector 内核也可能已经启动，但不会接收微信事件。

### 14.3 配置修改后的重载

修改 Hermes 插件的 agent、scheduler、output、data_dir 或鉴权配置后，执行：

```text
/pm reload hermes
```

最稳妥的做法是把 Hermes 插件配置变更视为需要 reload。仅依赖 `config.toml` 文件热注入不会重建已经启动的 Gateway listener 和 worker pool。

修改 Hermes Agent `.env` 或 `config.yaml` 后执行：

```bash
hermes gateway restart
```

## 15. 全链路验收步骤

按顺序执行并记录日志。

### 15.1 文本私聊

向机器人私聊发送：

```text
只回复：HERMES_OK
```

预期：

1. Golem OnEvent 把消息写入 `hermes.db`。
2. Turn 进入 Interactive lane。
3. Gateway 收到 inbound frame。
4. Hermes Agent 返回 final send。
5. Go 在事务中写入 Outbox。
6. Outbox Dispatcher 发送微信消息。
7. 收到 `HERMES_OK`。

### 15.2 群聊 @

在群聊中明确 @ 机器人：

```text
@Hermes 只回复：GROUP_OK
```

推荐的 `social_mode = "agent"` 下，@/引用会标记为 `group addressed` 并要求可见回复；未 @、未引用的消息标记为 `group ambient`，仍立即进入共享群会话，由 Hermes 自己决定参与或安静观察。

### 15.3 引用消息

引用机器人的上一条消息并提问。预期该消息被识别为明确交互并进入 Interactive lane。

### 15.4 图片

发送一张普通微信图片并提问。预期：

- OnEvent 只保存可恢复下载凭据，不在回调中下载。
- worker 获得 Run lease 后调用 Golem `message.Ability.Download`。
- 图片按 SHA-256 保存到 `<data_dir>/media/`。
- Hermes Gateway 收到可访问的本地绝对路径。

如果文本成功但图片失败，优先检查 Golem 与 Hermes 是否运行在不同文件系统或不同容器路径。

### 15.5 表情

入站验收：发送一个微信表情。预期 Inbox 将其记录为 `emoji`/`sticker` 媒体并传给 Gateway。部分微信表情只带 CDN URL，其可用性取决于 URL 是否仍有效以及 Hermes 所在机器能否访问该地址。

出站验收需要先完成 7.5 和 11.3。测试时可以在 Owner 私聊明确要求“一次只用表情回复”，确认描述为空时 Agent 调用 `golem_sticker_search`、`golem_sticker_inspect`、`golem_sticker_select`，微信收到的表情与视觉分析一致且看不到内部 completion token；再要求“先写一句话，再附一个合适的表情”，确认顺序为文字在前、表情在后。验收完成后恢复正常人格，让 Agent 自主决定使用比例。

检查 Outbox，不要直接打印包含媒体 Base64 的完整 payload：

```bash
sqlite3 -header -column data/hermes/hermes.db "
SELECT
  datetime(created_at / 1000, 'unixepoch', 'localtime') AS time,
  run_id,
  kind,
  state,
  attempt,
  length(payload) AS payload_bytes,
  last_error
FROM outbox
WHERE kind = 'emoji'
ORDER BY created_at DESC
LIMIT 10;
"
```

预期 `kind=emoji`，成功后 `state=sent`。媒体字节在选择阶段已经下载并写入 Outbox payload，因此后续重试不依赖第三方 URL 是否继续有效。

### 15.6 Hermes 命令与重置确认

先验证状态命令：

```text
/hermes status
```

预期 Hermes 返回当前会话、模型和上下文状态。然后发送：

```text
/hermes reset
```

Hermes 默认启用破坏性命令确认时会返回 `Confirm /new`。继续发送以下三者之一：

```text
/hermes approve
/hermes always
/hermes cancel
```

预期分别为单次批准、批准并永久关闭同类确认、取消本次确认。三条消息必须由 Owner 发送，并且必须与 `/hermes reset` 位于同一个微信私聊或群聊 session。

`/hermes cancel` 对应 Hermes 确认流程的 `/cancel`，不是 Golem 本地 Control Lane 的运行中断命令，不要把两种语义混用。

### 15.7 重启恢复

在有待发送 Outbox 或运行任务时停止并重启 Golem。预期：

- running Run 进入可重试状态。
- cancel_requested Run 收敛为 cancelled。
- 普通 leased Outbox 被恢复或在 lease 过期后重新取得。
- 视频 leased Outbox 在重启或 lease 过期后进入 dead_letter，不重新发送。
- 已提交的普通回复不会静默消失；结果不确定的普通发送可能重复，符合 at-least-once。

## 16. 数据与日志位置

默认插件数据：

```text
<Host 工作目录>/data/hermes/
  hermes.db
  media/
```

主要 SQLite 表：

- `inbox_events`
- `turns`
- `runs`
- `outbox`
- `delivery_attempts`

安装了 `sqlite3` 时可以查看积压：

```bash
sqlite3 data/hermes/hermes.db "select state, count(*) from runs group by state;"
sqlite3 data/hermes/hermes.db "select state, count(*) from outbox group by state;"
```

Hermes Agent 日志位于 `HERMES_HOME` 下的 `logs/`。Windows 默认是 `%LOCALAPPDATA%\hermes\logs\`，Linux 默认是 `~/.hermes/logs/`。

## 17. 更新插件

### 17.1 Linux/macOS

```bash
cd /path/to/golem
git pull
cd plugins/hermes
go test ./...
go build -trimpath -ldflags '-s -w' -o ../../host/plugins/golem_plugin_hermes .
chmod +x ../../host/plugins/golem_plugin_hermes
```

然后由 Owner 执行：

```text
/pm reload hermes
```

### 17.2 Windows

Windows 运行中的 `.exe` 通常不能直接覆盖。建议：

1. 先构建到 Host 插件目录之外的临时文件，避免 Host 把临时文件识别成另一个插件。
2. `/pm unload hermes`。
3. 替换正式文件。
4. `/pm load hermes`。

```powershell
Set-Location C:\path\to\golem\plugins\hermes
go test ./...
go build -trimpath -ldflags '-s -w' -o "$env:TEMP\golem_plugin_hermes.exe" .
```

发送：

```text
/pm unload hermes
```

然后：

```powershell
Move-Item -Force "$env:TEMP\golem_plugin_hermes.exe" ..\..\host\plugins\golem_plugin_hermes.exe
```

最后发送：

```text
/pm load hermes
```

## 18. 常见故障

### 18.1 Host 找不到插件

检查：

- 文件名是否以 `golem_plugin_` 开头。
- Windows 是否为 `.exe`。
- Linux 文件是否有执行权限。
- 插件是否位于 Host 当前工作目录的 `plugins/` 或其子目录。
- 是否误把文件放到了源码 `plugins/hermes/`，但 Host 实际从 `host/plugins/` 加载。

### 18.2 `address already in use`

端口 `8789` 已被其他 Hermes 实例或旧插件进程占用。停止旧进程，或为单个实例选择新端口，并同步修改 `GATEWAY_RELAY_URL`。

一个 connector 只允许一个 Gateway WebSocket 连接。不要让多个 Hermes profile 同时连接同一监听地址。

### 18.3 Gateway 不连接或持续重连

检查：

```bash
hermes gateway status
hermes doctor
```

并确认：

- Golem connector 先启动。
- `GATEWAY_RELAY_URL` 指向正确主机和端口。
- URL 使用 base URL，不要写成错误的重复路径，例如 `/relay/relay`。
- 本机防火墙允许连接。
- Hermes 安装包含 messaging/WebSocket 依赖；标准安装器默认包含。

### 18.4 401 Unauthorized

- 插件和 Hermes 的 Gateway ID/secret 不一致。
- 只配置了 ID 或只配置了 secret。
- 修改 `.env` 后没有重启 Gateway。
- secret 前后包含空格或引号内容不一致。

### 18.5 409 / relay already connected

已有另一个 Gateway 连接。运行：

```bash
hermes gateway list
hermes gateway stop --all
```

然后只启动需要的 profile。

### 18.6 微信消息进入 Golem，但没有回复

按顺序检查：

1. `[hermes] enable = true`。
2. `/pm list` 中 Hermes 为启用。
3. Gateway 已连接。
4. `hermes` CLI 本身能调用模型。
5. `social_mode = "agent"` 下，普通群消息无回复可能是 Agent 主动观察；用 @/引用或私聊验证必须回复的路径。
6. 查看 Hermes 是否按 descriptor 返回了内部观察 token。它只允许用于 `group ambient`，插件会成功完成 Run 但不创建聊天 Outbox。
7. 查看 `runs.last_error` 和 Outbox 状态。

### 18.7 收到 `The request failed temporarily...`

该文本不是 Hermes 或微信返回，而是插件 `0.4.1` 及更早版本在 Run 永久失败时生成的旧兜底。最常见根因是 Relay Run 从路由入队时就开始计算 `agent.timeout_seconds = 120`，排队和执行合计超过 120 秒后 Connector 会先于仍在工作的 Hermes 放弃任务。

升级到插件 `0.4.2` 或更高版本。Relay 不再使用插件墙钟 Deadline，任务活性由 Hermes `config.yaml` 的 `agent.gateway_timeout` 管理；Gateway 断线保留任务重试，内部失败不再生成聊天 Outbox。升级前已经发送或已经终止的历史 Run 只保留作诊断，不会自动重放陈旧群消息。

### 18.8 回复重复

本插件明确采用 at-least-once。微信已收到消息但 SDK 回执丢失时，Outbox 无法判断是否成功，会重试并可能产生重复。这是“不静默丢消息”的可靠性取舍。

### 18.9 图片失败但文本正常

- Golem 与 Hermes 不在同一文件系统。
- Windows Golem 搭配 WSL/Linux Hermes，Gateway 无法解析 `C:\...` 路径。
- Docker 未把 media 目录挂载为两端完全相同的路径。
- 微信下载能力未注入或原始图片已经过期。
- 图片超过 16 MiB或格式不受支持。

### 18.10 配置修改没有生效

- 修改 Golem `plugins/config.toml` 后执行 `/pm reload hermes`。
- 修改 Hermes `.env` 或 `config.yaml` 后执行 `hermes gateway restart`。
- 确认编辑的是当前 Host/Gateway profile 使用的文件，不是另一个工作目录或另一个 `HERMES_HOME`。

### 18.11 依赖下载失败

Windows PowerShell：

```powershell
$env:HTTP_PROXY = 'http://127.0.0.1:7897'
$env:HTTPS_PROXY = 'http://127.0.0.1:7897'
go mod download
```

Linux/macOS：

```bash
export HTTP_PROXY=http://127.0.0.1:7897
export HTTPS_PROXY=http://127.0.0.1:7897
go mod download
```

代理地址必须从执行命令的系统可达；远程服务器的 `127.0.0.1` 指向远程服务器自身，不是你的本地电脑。

### 18.12 表情工具未出现或选择失败

工具未出现时按顺序检查：

1. `/root/.hermes/plugins/golem_sticker_capabilities/` 同时包含 `plugin.yaml` 和 `__init__.py`。
2. `plugins.enabled` 使用 manifest 名 `golem-sticker-capabilities`，`platform_toolsets.relay` 包含 toolset 名 `golem_stickers`；这两个名称不同。
3. Hermes `.env` 中存在 `GOLEM_CAPABILITIES_TOKEN`，并已重启 Gateway。
4. Golem 配置中 `capabilities.sticker.enabled = true`，`capabilities.environment_file` 存在且其中令牌与 Hermes 侧相同，并已执行 `/pm load hermes`。
5. 使用 11.2 的 Python 命令确认 Relay 运行时确实解析出 `golem_stickers`，不要使用 v0.18.2 不支持 Relay 的 `hermes tools list --platform relay` 判断。

搜索返回暂时不可用时，检查 Golem 日志中的 `sticker search failed`，再核对 `capabilities.environment_file` 中的 `APIHZ_ID/APIHZ_KEY`、服务器外网和 DNS、APiHz 业务 `code`、本地每分钟限速。Provider 搜索请求可使用 Golem 进程的标准 `HTTP_PROXY/HTTPS_PROXY`；Hermes 到本地 Capability API 的控制面请求刻意忽略代理。

选择返回不可用通常表示候选已过 300 秒、候选属于另一个 Run、媒体主机不在 `allowed_media_hosts`、DNS 解析到私网、下载发生越权重定向、图片超过上限或 magic bytes 不是支持的图片格式。插件不会把这些内部错误伪装成微信回复；未成功选择时也不会创建 Emoji Outbox。

## 19. 安全检查清单

- [ ] Golem Owner 已配置。
- [ ] `relay_shared_secret` 使用至少 32 字节随机值。
- [ ] Hermes `.env`、Capability 凭据文件、`plugins/config.toml` 和数据库文件权限仅限服务账户。
- [ ] 跨主机 relay 使用 WSS/TLS。
- [ ] relay 的 `platform_toolsets` 只包含明确需要的 `web`、`file`、`skills`、`no_mcp`，不包含 `terminal` 或未授权 MCP。
- [ ] `file` 只在专用 Golem Relay profile 中启用，Gateway 服务账户对规则文件和 Skill 目录之外的路径使用最小必要权限。
- [ ] 若启用表情，Relay 额外开放 `golem_stickers`，Capability token 与 Provider 凭据来自权限受限的插件凭据文件。
- [ ] APiHz 或其他 Provider 的明文密钥没有写入 `plugins/config.toml`、URL、Git 或日志；已泄露的旧 Key 已轮换。
- [ ] Hermes 第三方插件没有给 relay 自动注入副作用工具。
- [ ] Golem 与 Hermes 共享媒体目录时只开放必要权限。
- [ ] 备份 `hermes.db` 前先安全停止插件，或使用 SQLite 在线备份工具。
- [ ] Hermes Agent 升级后重新执行文本、图片、取消和恢复测试。

## 20. 开发与验证

在插件目录执行：

```bash
gofmt -w .
go test ./...
go vet ./...
go test -race ./...
```

`go test -race` 需要启用 CGO 并安装 C 编译器。没有 CGO 的环境仍可运行普通测试和 `go vet`，但不能替代 CI 中的 Race Detector。

更多协议和架构细节：

- [GATEWAY.md](./GATEWAY.md)
- [DESIGN.md](./DESIGN.md)
