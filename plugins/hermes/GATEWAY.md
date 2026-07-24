# Hermes Gateway 接入

本文基于 2026-07-14 查询到的 Hermes Agent 官方资料：

- [消息网关中文文档](https://hermesagent.org.cn/docs/user-guide/messaging)
- [NousResearch/hermes-agent](https://github.com/NousResearch/hermes-agent)
- [Relay Connector Contract v1](https://github.com/NousResearch/hermes-agent/blob/main/docs/relay-connector-contract.md)

官方 Gateway 已提供 generic relay adapter，因此 Golem 不需要伪装成 OpenAI chat client，也不需要修改 Hermes Agent 核心源码。Hermes Gateway 主动连接 Golem connector；同一条 WebSocket 双向承载 inbound、outbound 和 interrupt。

## 1. Golem 插件配置

默认值已经可以用于仅本机部署：

```toml
[hermes.config.agent]
mode = "relay"
relay_listen = "127.0.0.1:8789"
relay_path = "/relay"
silence_rules_file = "/opt/software/wechat/data/hermes/workspace/silence-rules.txt"
```

`silence_rules_file` 可选。启用时文件必须在插件加载前存在且格式正确；Connector 会在每次最终回复时重新读取，因此 Hermes 修改文件后下一条消息即生效，无需重启。文件忽略空行和 `#` 注释，每行只能使用 `exact:`、`prefix:` 或 `suffix:`，匹配不区分大小写。例如：

```text
# Provider 渲染出的静默说明
exact:[silence]
prefix:I don't need to respond to this
```

文件上限为 64 KiB、256 条规则。运行中若文件丢失或格式错误，Connector 会记录 `silence rules reload failed`，当前文本不会被当成静默，Run 仍会正常流转。

需要跨主机监听时必须启用 HMAC 鉴权；代码会拒绝无鉴权的非 loopback 地址：

```toml
[hermes.config.agent]
mode = "relay"
relay_listen = "0.0.0.0:8789"
relay_path = "/relay"
relay_gateway_id = "golem-hermes"
relay_shared_secret = "replace-with-a-long-random-secret"
```

不要把 `relay_shared_secret` 提交到版本库。

## 2. Hermes Gateway 配置

在 Hermes Agent 的 `.env` 中设置：

```dotenv
GATEWAY_RELAY_URL=http://127.0.0.1:8789
GATEWAY_RELAY_ID=golem-hermes
GATEWAY_RELAY_SECRET=replace-with-the-same-long-random-secret
RELAY_HOME_CHANNEL=disabled
```

仅本机且 Golem 未配置 shared secret 时，只需要 `GATEWAY_RELAY_URL`。Gateway 会把 HTTP URL 规范化成 `ws://.../relay`。

Hermes Agent 的 `config.yaml` 应关闭 relay 侧 MCP。`relay` 没有官方核心 toolset，`no_mcp` 同时阻止全局 MCP 自动注入：

```yaml
group_sessions_per_user: true

agent:
  gateway_timeout: 1800
  gateway_timeout_warning: 900

platform_toolsets:
  relay:
    - delegation
    - cronjob
    - web
    - file
    - skills
    - no_mcp

display:
  tool_progress: off
  busy_input_mode: queue
  busy_ack_enabled: false
```

`web` 保留 Agent 搜索能力；`file` 与 `skills` 用于读取、维护 `silence_rules_file` 和 `~/.hermes/skills/` 下的 Skill。不要给 relay 开放 `terminal`。微信发送等平台副作用仍只能经 Go Capability Broker 授权；`file` 会继承 Hermes Gateway 服务账户的本地文件权限，因此只应在专用 Relay profile 中启用。

`delegation` 开放 Hermes 原生子代理工具。若 Golem `agent.async_delivery_enabled=true` 且 Hermes `.env` 设置 `GOLEM_ASYNC_DELIVERY_ENABLED=true`，后台子代理完成后会通过稳定 ticket 提交到 Transactional Outbox；该路径不放宽普通 orphan Relay send。完整约束见 [ASYNC_DELEGATION_DESIGN.md](./ASYNC_DELEGATION_DESIGN.md)。

相同开关也启用 Hermes `0.18.2` cron 主动投递。cron 创建时必须处于真实 Golem Relay Turn，插件把 `job_id` 绑定到当前微信会话；触发结果经 Bearer 鉴权的 capability HTTP 直接进入 Transactional Outbox，不要求 active Run，也不放宽普通 Relay orphan send。升级前创建的 cron 没有该绑定，必须删除后从目标微信会话重新创建。

推荐使用插件侧 `routing.social_mode = "hybrid"`，并将 Hermes 的
`group_sessions_per_user` 设为 `true`。群 observation stream 仍按微信群共享，只有
interactive transcript 按发送者隔离；因此 Hermes 能理解群里刚才的讨论，但不会把甲的
工具结果和长期对话串到乙的会话里。普通消息先经过快速规则和独立 SocialDecider：
`ignore` 只推进有序游标而不进入 transcript，`observe` 写共享观察，只有高置信
`respond` 才启动 Hermes。`agent` 保留全部普通消息直接交给 Hermes 的完全自主模式，
`mentions` 则只处理私聊、@机器人或引用机器人。

Hybrid 中未被点名但 SocialDecider 判定为 `respond` 的 ambient 回合保持固定工具
schema，但执行权限会收紧：允许文本与只读 web，拒绝 `delegate_task`、全部
`golem_sticker_*` / `golem_video_*` 工具及最终回复中的附件语法。权限来自认证 Relay
invocation 的 task-local trigger binding，不读取模型参数或环境变量。私聊、@Hermes、
引用 Hermes 和 control 回合不受此限制。Golem Capability Broker 还会基于 active Run
再次拒绝 ambient 媒体请求，确保即使绕过 Hermes hook 也不会调用 Provider 或创建 job。

插件 `0.4.2` 起，Relay 模式忽略 Golem `agent.timeout_seconds`，不再用墙钟 Deadline 提前放弃仍在运行的 Hermes Turn。任务活性由 Hermes `agent.gateway_timeout`（无活动超时，`0` 表示无限）管理；Relay 断线会保留 Run 并退避重试。插件内部失败只写 SQLite 和日志，不再生成 `The request failed temporarily. Please try again later.` 之类的微信聊天回复。

`RELAY_HOME_CHANNEL=disabled` 仅用于专用 Relay profile，抑制 Hermes 首轮会话的 Home Channel 引导；它不是微信目标 ID。Hermes v0.18.2 的 `hermes tools` CLI 静态白名单不包含动态 Relay，不能使用 `hermes tools list --platform relay`；运行时仍会正确读取 `platform_toolsets.relay`。可用部署手册中的 Hermes Python 运行时解析命令核验最终工具面。`no_mcp` 不负责禁用 Hermes 插件 toolset。未授权的插件 toolset 只放入 `known_plugin_toolsets.relay`，不要放入 `platform_toolsets.relay`；需要启用 0.5.0 表情能力时则显式把受控的 `golem_stickers` 放入后者。

## 3. 可选表情 Capability

当前表情 Capability 在同一个 HTTP listener 上提供三个 Bearer 鉴权端点：

```text
POST /capabilities/v1/stickers/search
POST /capabilities/v1/stickers/materialize
POST /capabilities/v1/stickers/select
```

Hermes 用户插件从 task-local `gateway.session_context` 注入当前 Relay 上下文，模型参数中没有 wxId、chat ID、URL 或路径。Search 只返回随机候选 ID；Materialize 再次验证当前活动 Run 并返回 Golem 安全下载的候选字节，由 Hermes `auxiliary.vision` 识别；Select 复用完全相同的缓存字节并暂存 Emoji effect。最终 Relay send 到达后，文字和 effect 才一起提交到 Transactional Outbox。ambient 回合在 Search/Materialize/Select 之前即被拒绝，不会产生需要事后丢弃的 effect。

Provider 是可配置的 HTTP/JSON 驱动，支持 GET query、POST form、`${env:NAME}` 凭据模板、GJSON 响应路径和有限的纯数据转换。Capability Bearer token 和 Provider 模板变量都从 `capabilities.environment_file` 读取，并随 `/pm load hermes` 生效，不要求修改、停止或重启 Golem Host。Host 环境变量读取仅为旧部署兼容。下载阶段强制 HTTPS、媒体域名白名单、Public IP、重定向复验、大小上限和图片 magic 校验。完整 APiHz `type=2` 配置、Hermes 用户插件安装和环境变量见 [readme.md](./readme.md)。

Golem Host 会先消费 `/new`、`/reset` 一类斜杠命令。Hermes 插件通过 `CommandPlugin` 注册 `/hermes`，微信侧使用 `/hermes status`、`/hermes reset`、`/hermes approve` 等 Owner 命令；命令先进入 Durable Inbox，只在 Worker 到 Relay 的边界转换为 `/status`、`/reset`、`/approve`。旧版 `hermes:new` 和 `hermes:reset` 文本桥接仍保留兼容。

## 4. 启动顺序

1. 启动或启用 Golem Hermes 插件，确认 connector 已监听。
2. 运行 `hermes gateway` 前台验证，或运行 `hermes gateway start` 启动服务。
3. 查看 Hermes 日志，确认 relay descriptor 为 `Golem WeChat`、contract version 为 `1`。
4. 从微信发送一条私聊消息，或在 `social_mode = "agent"` 的群内发送普通消息；再由 Owner 发送 `/hermes status` 验证命令链路。

常用官方命令：

```bash
hermes gateway status
hermes gateway start
hermes gateway stop
```

## 5. Wire contract

Gateway -> Golem：

- `hello`
- `outbound`（`send`、`typing`、`get_chat_info`）
- `interrupt`
- `going_idle`

Golem -> Gateway：

- `descriptor`
- `inbound`
- `outbound_result`
- `interrupt_inbound`
- `going_idle_ack`

`outbound_result.success=true` 表示 Golem 已接受输出提案，不表示微信已经发送成功。真实投递必须先经过 Run 成功事务和 Transactional Outbox；SDK 结果随后写入 DeliveryAttempt/receipt。

## 6. 当前 contract v1 限制

官方 relay v1 标记为 experimental，并且没有独立的 `turn_completed` 帧，也尚未定义 media outbound action。当前实现采用以下兼容策略：

- `send.metadata.notify=true` 视为最终回复，其他 send 视为 progress proposal。
- descriptor 声明不支持 draft/edit/thread，减少中间消息和修订语义。
- platform hint 要求私聊和 `group addressed` 给出可见最终回复；若 Hermes 对任意群消息最终仍返回 Connector 专用观察 token，Connector 都以无 Outbox 的成功 Run 完成，避免模型偏差阻塞 Session。部分 Provider 会把该决定渲染成完整括号式说明（例如 `[... — staying silent]` 或 `[silence]`），Connector 仅在整条群聊最终回复明确为这类静默说明时作同样处理。其他 Provider 特有文本通过 `silence_rules_file` 管理。私聊不接受观察 token，正文中提及 silent/no reply 也不会被吞。
- Gateway 输出图片/表情的标准 relay action 尚未发布。0.5.0 的受控 Hermes 工具只能暂存结构化 Emoji effect，最终仍由 Relay send 完成 Run，并由 Go Outbox 发送。
- 输入图片优先使用 Host 原始事件中的缩略图字节；没有内嵌字节时，Adapter 把可恢复下载凭据写入 Inbox，worker 获得 Run lease 后通过入站 `cdn.Ability.DownloadImage` 下载，并保留 `message.Ability.Download` 作为 web 模式兼容回退。若 Host CDN 暂时不可用，只有经过 allowlist 校验的微信官方 `cdnurl` 才可作为最后回退，并继续执行大小与图片 magic 校验。GIF 会在交给视觉模型前转换为第一帧 PNG，以兼容只接受 JPG/PNG 的模型。Gateway 最终收到 `data/hermes/media` 下按 SHA-256 命名的本地文件路径或经过域名校验的微信媒体 URL。
- 独立图片/表情到达时只写入 Inbox 和观察上下文，不启动 Hermes Run，也不调用视觉模型。Worker 启动普通 Run 时不会根据文本关键词自动取图；Relay 暴露 `golem_image_search_current_session`（仅元数据）和 `golem_image_read_current_session`（按候选 ID 延迟物化）。Hermes Agent 自己先按发送者、消息 ID、时间筛选，再显式读取；读取时才完成 CDN/官方 URL 校验、GIF 首帧规范化并进入 Hermes 视觉通道。`image_input_mode=text` 只影响这次显式读取的视觉结果。

relay Gateway 必须与 Golem 共享该本地媒体目录的文件系统视图。当前默认部署为同机进程；跨主机部署需要把 `data/hermes/media` 挂载到两端相同路径，官方 contract v1 尚未提供 connector 到 Gateway 的媒体上传帧。

后续官方 contract 增加 turn completion、media 或 tool-call frame 时，应通过版本/能力协商增量接入，不改变 Durable Inbox、Go Broker 或 Outbox 的所有权。

## 7. 安全边界

- Gateway 只看到不透明 chat/session key，不看到微信 Receiver。
- loopback 以外的 relay 必须使用官方格式的短期 HMAC bearer token。
- HMAC 格式与官方 `gateway/relay/auth.py` 一致：`base64url(gateway_id:expires:hmac_sha256(gateway_id:expires))`。
- WebSocket 单帧上限 8 MiB。
- Agent 输出不能直接执行微信副作用，只能形成 proposal。
- 表情候选 ID 与 Run/chat 绑定且短时失效；Agent 看不到 Provider URL，也不能改变接收者。
- 网络图片只允许无凭据 HTTPS URL和配置允许的 CDN 域名，拒绝 loopback、私网、link-local、multicast、越权重定向、超限内容和非图片 magic。
- 出站图片和表情只通过 SDK `message.Send` 的 `Media.Data` 把原始字节交给 Host，由 Host 内部执行 `SendImage`/`SendEmoji` 和 CDN 上传；Hermes 不为出站媒体预上传。`cdn.Ability` 仅用于恢复入站微信图片/表情字节。
- 调用 Host 前失败使用 at-least-once 安全重试；一旦进入 Host 调用而 receipt 不确定，
  Outbox 标记为 `ambiguous` 且不自动重发，避免群里出现重复视频。
