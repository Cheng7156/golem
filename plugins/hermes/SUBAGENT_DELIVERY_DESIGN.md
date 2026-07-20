# Hermes 子代理多类型异步投递设计

状态：设计草案
分支：`codex/hermes-subagent-media-delivery`
版本基线：Hermes Agent `0.18.2 / v2026.7.7.2`

## 1. 背景

当前 Golem 已支持 Hermes 后台子代理结果回流，但链路是 text-only：

```text
delegate_task(background=true)
  -> Python async delivery bridge
  -> POST /capabilities/v1/async-delivery/deliver { content }
  -> Go CommitAsyncDelivery
  -> outbox(kind="text")
  -> message.Send
```

表情能力则是 active Relay Run 内的 staged effect：

```text
golem_sticker_search
  -> golem_sticker_select
  -> relayRun.stageEffect(kind="emoji")
  -> final Relay send
  -> EventEffectProposed
  -> outbox(kind="emoji")
```

后台子代理 completion 到达时，父 Run 已经终态，普通 Relay `send` 必须继续拒绝 orphan send。因此不能通过放宽 `no active run for chat` 来解决。

## 2. Hermes v0.18.2 约束

从 Hermes 当前文档和本地桥接代码确认：

- https://hermes-agent.nousresearch.com/docs/guides/delegation-patterns/
- https://hermes-agent.nousresearch.com/docs/zh-Hans/guides/delegation-patterns
- https://github.com/NousResearch/hermes-agent/blob/main/website/docs/user-guide/features/delegation.md

- 子代理继承父会话已启用 toolsets，模型不能在调用时扩大能力。
- leaf 子代理不能调用 `send_message`，也不能继续 `delegate_task`。
- 顶层 background delegation 可以稍后把结果重新发送回对话，但执行本身仍绑定 Hermes 进程和所属会话。
- 子代理最终只有 summary 回到父上下文；后台 completion 由 Hermes Gateway 注入 synthetic turn。

这意味着：Golem 需要把“可投递副作用”建成受控 capability，而不是依赖 Hermes 原生 messaging tool。

## 3. 目标

1. 后台子代理可以投递文本、表情，后续可扩展图片、视频。
2. 主对话 Run 不等待子代理完成，不阻塞后续用户消息。
3. 不放宽普通 Relay orphan send。
4. 所有输出先进入 Transactional Outbox，再由现有 dispatcher 发送。
5. 多条输出保持同 session sequence 顺序，复用 receiver 级发送节流。
6. 失败显式暴露为 capability/Run/Outbox 错误，不静默降级成文本。

## 4. 非目标

- 不让子代理直接获得 wxid、receiver_id 或任意 chat target。
- 不允许子代理提交任意 URL 或本地路径让 Go 直接下载发送。
- 不在 Hermes Python 侧调用 Golem SDK、CDN 或微信 API。
- 不在 Relay v1 上伪造新的 media outbound action。
- 不把拼接文本协议作为长期媒体协议。

## 5. 核心设计

把 async delivery 从 `content string` 升级为输出 envelope：

```json
{
  "outputs": [
    {"kind": "text", "payload": {"content": "处理好了"}},
    {"kind": "emoji", "payload": {"data": "...base64...", "mime_type": "image/png", "description": "开心"}}
  ]
}
```

Go 侧只接受结构化、已授权、已绑定 ticket 的输出。每个 output 转成一个 `OutboxDraft`，在同一个 SQLite 事务内提交。

稳定性原则：envelope 绝不能要求模型在最终回复里手写 JSON。模型只允许调用受控工具；Python 桥接脚本维护本轮 `OutputBuilder`，由脚本把工具结果组装为 `outputs[]`。Go 侧再按 schema、ticket、scope、MIME、大小和顺序二次校验。

```text
模型意图
  -> tool call: golem_sticker_attach(candidate_id)
  -> Python handler validates and materializes
  -> OutputBuilder.append(kind="emoji", payload=...)
  -> final text returns from Hermes
  -> Python bridge appends optional text output
  -> deliver-v2(outputs)
  -> Go validates again
  -> Transactional Outbox
```

禁止路径：

- 不解析模型最终文本里的 JSON 作为权威输出。
- 不从 Markdown 图片、`MEDIA:`、本地路径或 URL 推断媒体输出。
- 不把未知 token 或不完整结构静默降级成文本。
- 不允许模型传入 receiver、wxid、chat id 或文件路径。

## 6. 协议版本

保留现有 `/capabilities/v1/async-delivery/deliver` 文本协议，新增：

```text
POST /capabilities/v1/async-delivery/deliver-v2
```

请求：

```json
{
  "ticket": "adt_...",
  "delegation_id": "deleg_...",
  "producer_epoch": "ade_...",
  "hermes_session_id": "...",
  "relay_session_key": "...",
  "chat_id": "...",
  "profile": "...",
  "outputs": []
}
```

响应沿用：

```json
{
  "state": "consumed",
  "disposition": "delivered",
  "message_id": "async_...",
  "outbox_ids": ["outbox_..."]
}
```

旧 `outbox_id` 可在 v2 响应里保留为第一条 outbox，便于兼容监控。

## 7. 输出类型

### text

```json
{"kind":"text","payload":{"content":"..."}}
```

Go 侧继续使用 `newRelayTextProposal` / Markdown-to-WeChat 清洗。

### emoji

```json
{
  "kind": "emoji",
  "payload": {
    "data": "base64 bytes",
    "mime_type": "image/png",
    "md5": "...",
    "description": "..."
  }
}
```

来源必须是 Golem sticker capability 物化出来的 bytes。Python 插件不能提交 provider URL。

### image

```json
{
  "kind": "image",
  "payload": {
    "data": "base64 bytes",
    "mime_type": "image/png",
    "alt": "..."
  }
}
```

首期可以只允许 Golem 自有 renderer 或未来受控 image capability 产物，不接受模型任意 URL。

### video

```json
{
  "kind": "video",
  "payload": {
    "object_id": "media object id",
    "thumb_object_id": "thumbnail object id",
    "duration": 12,
    "title": "..."
  }
}
```

视频对象和缩略图写入 Golem 媒体对象存储，Outbox 只保存对象 ID。Dispatcher
读取对象并通过 `message.TypeVideo` 发送；发送成功或进入死信后释放 Outbox 引用。

## 8. 子代理表情工具改造

当前 `golem_sticker_select` 只能把 effect 暂存在 active `relayRun`，后台 completion 没有 active run，必须拆成两类工具语义：

1. `golem_sticker_select`
   - 仅 active Relay Run 可用。
   - 行为保持不变：stage effect，最终由 Relay send 提交。

2. `golem_sticker_attach`
   - async completion turn 可用。
   - 输入仍是 `candidate_id`。
   - Go 侧用 ticket binding 而不是 active run 校验 scope。
   - 每个 invocation 直接写入绑定会话的 Emoji Outbox。
   - 返回 `queued=true`、Outbox ID 和会话序号；completion 不重复投递。

视频子代理使用同样的直接投递语义：`golem_video_search` 获取不透明候选，
`golem_video_attach` 准备媒体后直接写入 `kind="video"` Outbox。视频准备采用
后台 job 和 status 轮询，避免 HTTP 请求在 ffmpeg 运行期间失去响应。

首期可以复用现有 materialize cache，但 scope 要扩展为：

```text
active run scope: run_id + chat_id
async scope: ticket_hash + chat_id + delegation_id
```

## 9. Python 桥接改造

当前 async completion 仍可通过 `deliver-v2` 回流文本；工具产生的 Emoji 和视频
使用 Direct Output 端点独立入队，不依赖 OutputBuilder，也不等待父 Relay completion。

Direct Output 的可信上下文由脚本和 Golem middleware 提供，不暴露给模型：

```text
child tool call
  -> current_delivery = binding
  -> middleware supplies invocation_id
  -> Golem validates candidate and ticket scope
  -> Direct Output transaction creates one Outbox item
  -> completion is suppressed when direct_output_count > 0
```

规则：

- 如果只有直接媒体，允许 text 为空。
- 如果有未知内部 token，提交失败，不转成文本。
- 如果没有任何 output，失败暴露为 CapabilityError。
- async completion turn 继续禁止普通 Relay send。
- `async_delivery_text.sanitize_text` 从“删除媒体”改成“只清理非结构化媒体痕迹”；结构化媒体走 outputs。

所有 Direct Output 只提供脚本 API：

```python
golem_sticker_attach(candidate_id)
golem_video_attach(candidate_id)
```

模型看不到这个 builder，只能调用注册工具。每个工具 handler 负责：

1. 从当前 ContextVar 读取 binding 和 builder。
2. 校验当前是 async completion turn。
3. 调用 Golem capability 取得受控 bytes 或结构化 payload。
4. 提交 Direct Output 并返回短文本工具结果。

工具结果不能携带 base64 媒体，也不能让模型复制 payload。

### 9.1 稳定性分层

| 层级 | 机制 | 用途 | 是否可投递 |
| --- | --- | --- | --- |
| L1 | 脚本工具 Direct Output | emoji/video 正式输出 | 是 |
| L2 | Hermes final text | 普通文字补充 | 是，脚本清洗后 append text |
| L3 | 模型手写 JSON/Markdown/MEDIA | 诊断或用户可见文本 | 否 |

任何 L3 内容如果看起来像内部协议，直接报错，不做猜测修复。

## 10. Go 提交改造

新增领域结构：

```go
type AsyncOutput struct {
    Kind string
    Payload json.RawMessage
}

type AsyncDeliveryCommit struct {
    ...
    Outputs []AsyncOutput
    Silent bool
}
```

提交流程：

1. 校验 ticket binding。
2. 校验每个 output kind 和 payload。
3. 写 async synthetic inbox/turn/run 审计链。
4. 为每个 output 生成 outbox item。
5. 按 session 当前 max(sequence) 追加顺序。
6. consumed ticket 记录第一个 message_id 和所有 outbox ids。
7. 唤醒 dispatcher。

`CommitRunSuccess` 已经证明多 `OutboxDraft` 是系统内建语义，async delivery 应复用相同转换规则。

## 11. 数据库兼容

现有 `async_delivery_tickets.outbox_id` 是单值。v2 可先保留：

- `outbox_id` 写第一条 outbox。
- 新增 `result_json` 或 `outbox_ids_json` 记录全部 outbox。

如果不想迁移 ticket 表，也可以只通过 `run_id = "run_"+ticket.ID` 从 outbox 反查全部结果；但状态接口会较弱。建议做 V4 migration 增加 `result_json`。

## 12. 安全边界

- 所有媒体 bytes 必须来自 Golem-controlled capability 或 renderer。
- 所有媒体都做 magic bytes、MIME、大小上限校验。
- v2 body 上限必须显式提高，但仍低于可控上限，例如 12 MiB。
- 单次 outputs 数量需要配置上限，默认 10。
- 不允许模型提交 target、receiver、wxid、URL、本地路径。
- ticket consumed 后幂等返回同一结果，不重复写 outbox。

## 13. 分阶段落地

### Phase 1：v2 文本等价

- 新增 `deliver-v2`。
- 支持 `outputs=[text]`。
- 与旧 deliver 行为一致。
- 测试幂等、revoke、abandon、silent、outbox 顺序。

### Phase 2：async emoji

- 新增 async sticker attach。
- Python 收集 emoji output。
- Go 提交 `kind="emoji"` outbox。
- 测试子代理只发表情、文本加多表情、reset 后丢弃。

### Phase 3：image

- 引入受控 image/render output source。
- Go 支持 async image payload。
- 与现有 `golemSender` image 分支复用。

### Phase 4：video（已落地）

- 配置 Provider 的 JSON URL、二进制流和自动识别模式。
- 视频下载、ffprobe 校验、必要时 ffmpeg 转码并生成缩略图。
- 子代理 `golem_video_attach` 通过 Direct Output 创建有序视频 Outbox。

## 14. 验收标准

- 主 Run 发起 background delegation 后立即完成，不等待子代理。
- 子代理完成后可发送 1 到 N 条 text/emoji outbox。
- 连续 5 个表情按 receiver 发送间隔逐条发送。
- 子代理不能通过普通 Relay send 发送。
- 子代理不能改变目标会话。
- Hermes `/stop`、`/new`、`/reset` 后 pending ticket 不再投递。
- Hermes 进程重启后未完成子代理不伪装成功；已提交 outbox 可恢复发送。
