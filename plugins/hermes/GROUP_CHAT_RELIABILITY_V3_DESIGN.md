# Hermes 群聊可靠性 V3 设计

状态：实施中；核心可靠性路径、生产部署与插件 reload 已完成，进入线上观察期

创建分支：`feat/group-chat-reliability-v3-20260723-125523`

当前分支已实现：root Gateway 单 boot producer epoch、completion `discarded` 终态、
SQLite durable video job、前台视频持久化 enqueue、确定性 JSON 候选选择、45 秒进度、
结构化 SocialDecider、`ignore/observe` 有序分流、1.5 秒有界 observation barrier，
`queued -> sending -> sent/ambiguous/dead_letter` 的诚实微信投递状态，以及按可信
trigger task-local binding 执行的 ambient 工具限权。生产二进制已原子替换并保留回滚副本，
用户插件与视频 skill 已同步，配置已切换到 hybrid/per-user session，Gateway 已重启；Golem
Host 仍保持原 PID 运行。16:53:55 UTC 收到可信微信 Owner 的 `/pm reload hermes`，旧插件
PID `765880` 正常退出，新插件 PID `797000` 从当前二进制启动；Golem 返回“插件已重载：hermes”。

部署记录（2026-07-23 16:52 UTC）：SQLite 在线备份位于
`/opt/software/wechat/deploy-backups/20260723_165106/` 与
`/root/.hermes/deploy-backups/20260723_165106/`；新 Golem 二进制 SHA256 为
`57abb5b558940504b7904522341dd70ee06ec1743fce5094f3b7c5ee236458db`，旧文件保留为
`golem_plugin_hermes.pre-group-context-compression.bak`；Host PID `727041` 未变化，Gateway
PID 为 `796845`（后续 reload 不影响 Gateway PID）。SQLite 两端 `PRAGMA integrity_check` 均为
`ok`，Relay 已重连，未发现本次启动错误。

## 1. 目标

本设计解决两类已经在线上发生的问题：

1. 用户明确要求 Hermes 获取并发送视频后，Hermes 回复“派下去”，但任务几十分钟没有结果；插件 reload 后任务还可能消失。
2. 群聊几乎所有普通消息都进入完整 Agent，造成上下文持续膨胀、任务互相取消、回复变慢，以及 Hermes transcript 修复和消息时序告警。

最终目标不是让 Agent “更积极”，而是让每条消息有明确、可恢复、可解释的处理路径：

```text
明确指令      -> 可靠任务 -> 可见进度 -> 微信真实投递结果
值得参与的话题 -> 交互会话 -> 有边界的共享群上下文
普通群消息    -> 结构化观察或忽略，不启动完整 Agent
```

## 2. 开工前代码保护边界

设计前已检查两个仓库的未提交代码。

### 2.1 Golem

- 原分支：`feat/hermes-url-video-fetch-delivery`
- 原 HEAD：`95ab300 feat(hermes): harden group chat delivery`
- 未暂存：25 个已跟踪文件，`430 insertions / 44 deletions`
- 未跟踪：5 个文件，共 754 行
- 无暂存内容，`git diff --check` 通过

这些改动已经实现任意公网 HTTP(S) URL 探测、SSRF 防护、JSON 候选抽取、视频下载/转码、媒体存储和 Outbox 接入。部署目录与这份未提交 Python 插件源码一致，因此它是当前生产行为，不是废弃草稿。

### 2.2 Hermes Agent

- 原分支：`feat/group-chat-reliability-v2`
- 原 HEAD：`fc0d51b30 feat(relay): harden group chat coordination`
- 未暂存：`gateway/run.py`、`tests/gateway/test_session_env.py`
- 变更量：`84 insertions / 2 deletions`
- 无暂存内容，`git diff --check` 通过

这份改动在递归/排队 follow-up 真正开始时重新绑定当前 sender 和 `session_id`，修复：

```text
HTTP 409: user context does not match the active run
```

### 2.3 必须保留与必须替换的部分

必须保留：

- URL、DNS/IP、重定向、响应大小和媒体格式的安全校验；
- 下载、ffprobe、必要时 ffmpeg、缩略图、不可变媒体对象；
- ticket 与 profile/session/chat/receiver/parent Run 的安全绑定；
- Direct Output、Transactional Outbox 和 invocation 幂等；
- Hermes follow-up 回合的 task-local sender/session 重绑定修复。

必须替换的首版假设：

- `golem_video_fetch` 只能由 `delegate_task(background=true)` 调用；
- 所有 JSON 都必须重新交给后台子 Agent 判断；
- 视频 job 和已探测 URL 可以只存进程内内存；
- 所有视频发送失败都直接死信且不给用户最终解释；
- 插件每次 install/register 都可以生成新 producer epoch 并 reconcile。

新增实现不得覆盖或重写现有未提交文件的语义，必须以增量迁移和测试锁定这些保护边界。

## 3. 已确认的线上证据

当前数据不是理论风险：

| 现象 | 已确认数据 | 直接后果 |
| --- | ---: | --- |
| 群消息进入完整 Chat | 6328 / 6516，97.1% | 大量无效模型调用 |
| Ambient Run | 6504 次 | 群聊背景消息占用主处理链 |
| Ambient 被取消 / 失败 | 452 / 242 次 | 计算浪费、时序噪声 |
| admission supersede | 日志 346 次 | 老消息运行到一半被新消息替代 |
| stale ambient expiry | 日志 74 次 | 排队后直接过期 |
| Hermes role alternation repair | 762 次 | transcript 顺序反复被修补 |
| transcript lag warning | 31 次 | 模型看到的上下文落后于群消息 |
| 群共享输入上下文 | 约 21K–23K tokens | 延迟、成本和注意力稀释 |
| 视频 Outbox 死信 | 12 / 44，27.3% | 用户只看到“派下去”而无结果 |
| DeadlineExceeded 视频死信 | 7 次 | 可恢复的瞬时问题被当永久失败 |

失败请求还出现过 5 次模型调用和约 69K input tokens。日志显示子进程在 07:56:19 加载插件时，把 07:56:07 创建的 ticket abandon；这与当前模块 import 随机生成 `PRODUCER_EPOCH`、每次 plugin install 都 reconcile 的行为一致。

## 4. 设计原则

1. **接单先持久化。** 只有 durable job 创建成功后，才能回复用户“已接单”。
2. **发送状态必须诚实。** `queued`、`sending` 和微信 `sent` 是不同状态，不能把进入 Outbox 叫 delivered。
3. **确定性工作不用 Agent。** URL 校验、唯一候选选择、下载、转码和投递均由代码完成。
4. **Agent 只处理语义歧义。** 多个可信候选无法确定时，最多调用一次隔离的 selector；不携带完整群聊 transcript。
5. **所有长任务可恢复。** plugin reload、进程退出和 lease 超时后从 SQLite 继续，不依赖 Python detached task 内存。
6. **安全重试，不盲目重发。** 明确未到达微信的阶段可重试；微信结果不确定时禁止自动重复发送。
7. **群聊先路由再推理。** 普通消息先做快速过滤和结构化社交判断，只有 `respond` 才启动完整 Agent。
8. **工具集稳定，权限按执行上下文校验。** 不在同一 session 中动态改 system prompt 或 tool schema。
9. **严格保持 role alternation。** 共享观察通过有序 barrier 注入，不能直接拼接破坏 user/assistant 顺序。

## 5. 新的端到端流程

### 5.1 明确的视频 URL 指令

示例：

```text
@Hermes 把这个视频发给我 https://example.com/api
```

流程：

```text
Golem 收到消息
  -> 快速路由识别“明确视频动作 + 单个 HTTP(S) URL + addressed/private”
  -> 在 active Run 内创建 durable video_fetch_job
  -> 同事务保存原会话、发送目标、幂等键和 producer owner
  -> job 创建成功后回复“已接单，正在解析视频”
  -> durable worker lease job
  -> inspect URL
       直接视频                  -> download
       文本中唯一 URL             -> download
       JSON 唯一高置信候选         -> download
       JSON 多个接近的可信候选      -> 一次 isolated selector
       无可信候选                  -> failed
  -> 下载 -> 探测 -> 转码 -> 缩略图 -> 媒体对象
  -> 幂等创建 video Outbox
  -> Dispatcher 调用微信发送
       明确成功        -> job delivered
       明确未发送      -> 安全重试
       发送结果不确定   -> ambiguous，不自动重发
       永久错误        -> failed
  -> 必要时发送明确失败说明
```

这个路径不需要 `delegate_task(background=true)`，因此不会再暴露给 detached 子代理生命周期、child plugin discovery 或 synthetic completion 队列。

### 5.2 不够明确的视频请求

例如用户只是说“看看这个链接”，或者同一消息有多个 URL。消息仍进入正常交互 Agent。Agent 可以调用快速返回的 `golem_video_fetch_enqueue`，但工具本身只负责创建 durable job，不在 Tool Call 内下载视频。

工具成功返回：

```json
{
  "accepted": true,
  "job_id": "vfj_...",
  "state": "queued",
  "deduplicated": false
}
```

Agent 只能在 `accepted=true` 后告诉用户任务已经建立。它不能说“已经发送”。

### 5.3 多候选 selector

只有以下条件全部满足才调用 selector：

- 文档经过大小、深度、字符串长度限制和敏感字段脱敏；
- 至少两个候选通过 URL 安全检查；
- 最高分与第二名分差小于配置阈值；
- 确定性规则无法得到唯一候选；
- 本 job 尚未调用过 selector。

selector 输入只包含用户这一句指令、脱敏后的局部 JSON path、候选 URL 的安全摘要和分数；不包含共享群聊历史、工具列表或其他会话。输出必须符合固定 schema：

```json
{"candidate_id":"...","confidence":0.91,"reason_code":"field_semantics"}
```

无合法结构、低置信度或超时都直接失败并提示用户，不递归调用 Agent，也不产生第二次模型选择。

## 6. 改动清单：问题、实现与体验变化

### 6.1 稳定 producer owner，禁止 child 触发 reconcile

**当前问题**

`PRODUCER_EPOCH` 在 Python 模块 import 时随机生成，且每次插件 install 都调用 reconcile。后台 child 加载插件会被 Golem 当作“新 Gateway”，从而 abandon 同一请求刚创建的 ticket。

**设计**

- Gateway 根进程启动时生成一次 `gateway_instance_id`；同一次 boot 的所有 profile 共用它。
- 根进程通过受锁 runtime state 文件把 instance id 提供给受信子进程；子进程只读，不得生成替代值。
- 只有 Gateway 根生命周期的 `startup_owner` hook 能调用 reconcile。
- Golem 增加 producer owner lease/heartbeat。reconcile 必须携带 owner generation，并以 compare-and-swap 接管；普通 plugin register、tool call 和 child discovery 没有权限。
- plugin reload 不改变 owner generation；Gateway 真正重启才产生新 generation。
- 旧 owner 只有 lease 确认过期且新 owner 成功接管后，pending ticket 才能进入 `abandoned`。

**具体解决**

消除“任务已经登记，但几秒后因 child import 被自己清理”的竞态，也避免 multiplex profile 重复 reconcile。

**用户体验变化**

用户看到“已接单”后，任务不会因为后台子模块加载或插件 reload 突然失踪。

### 6.2 建立诚实的投递状态机

**当前问题**

当前 `delivered` 有时只表示 Golem 接受了 completion 或建立了 Outbox；inactive/unregistered completion 路径还可能调用 `mark_completion_delivered`。这会让 Hermes 认为工作完成，而微信里什么也没收到。

**设计**

异步任务与投递状态分离：

```text
job: queued -> inspecting -> selecting -> downloading -> processing
     -> output_committed -> waiting_delivery -> delivered
     -> failed | cancelled

delivery: pending -> leased -> sent
          pending/leased -> retry_wait -> leased
          leased -> ambiguous | dead_letter
```

- capability HTTP 接受结果统一叫 `accepted` 或 `queued`，不叫 delivered。
- 只有 Dispatcher 获得微信成功 receipt 后，才把 delivery 和关联 job 标为 `sent/delivered`。
- orphan、inactive、unregistered completion 只能标为 `discarded` 或 `failed_registration`。
- ticket 保存 `terminal_reason`、`outbox_id`、`receipt_id` 和时间戳，可追溯每一步。

**具体解决**

消除假成功，日志、状态接口和用户看到的结果使用同一事实来源。

**用户体验变化**

“已接单”只表示任务存在；视频真正到群里/私聊后才算完成。失败时会得到明确失败消息，而不是无限等待。

### 6.3 明确 URL 指令走确定性 durable job

**当前问题**

简单的 URL 视频发送也被强制包装成后台子代理，经历父模型、delegate、子模型、工具和 synthetic completion。路径过长，任一环节出错都可能无结果。

**设计**

- 在 Golem route 前增加只识别高精度格式的 `video_url_command` 分类器。
- 仅对私聊，或群聊中明确 @/引用 Hermes 的消息生效。
- 分类器只提取动作、一个 URL 和可选标题；不猜测模糊语义。
- 命中后直接创建 durable job；未命中保持正常 Agent 行为。
- Agent 工具改为 enqueue 型快速接口，取消“必须 background child”的限制。

**具体解决**

把常见用例从多次模型和 detached delegation 缩短成数据库事务加后台 worker。

**用户体验变化**

明确指令通常在一两秒内收到真实接单确认，后续处理不占住聊天；其他聊天仍可正常继续。

### 6.4 唯一候选自动选择，歧义最多调用一次模型

**当前问题**

首版把所有 JSON 都返回给 child LLM。即使结构中只有一个明显视频 URL，也会再次调用模型；失败请求曾累计 5 次调用、约 69K 输入 token。

**设计**

- 保留现有候选提取和评分；新增明确阈值：只有一个候选或第一名达到绝对阈值且领先足够分数时自动选中。
- 相对 URL 先按最终响应 URL 解析，再重新走公网安全校验。
- 多候选只调用一次 isolated selector；其余失败交给用户，不递归。
- 将选择原因写入 job：`direct_media`、`single_candidate`、`score_winner`、`model_selector`。

**具体解决**

避免为确定性答案付出完整 Agent 成本，并杜绝 selector 反复自调用。

**用户体验变化**

常见 JSON API 的处理更快；复杂接口失败时会很快说明“无法确定哪个字段是视频”，不会长时间显示处理中。

### 6.5 持久化视频 job 与 inspected URL 授权

**当前问题**

`asyncVideoJobs` 和 `asyncVideoURLs` 在 Go 进程内存中，plugin reload 后全部丢失。正在轮询的 Python 工具只会得到 job not found。

**设计**

新增 SQLite 表：

```text
video_fetch_jobs
  id, idempotency_key, source_kind, source_url, title
  profile, session_id, receiver_id, chat_id, parent_run_id
  ticket_id nullable, producer_owner_id
  state, stage, attempt, max_attempts
  lease_token, lease_until, next_attempt_at
  selected_candidate_id, media_object_id, outbox_id
  failure_class, failure_code, failure_message
  created_at, updated_at, finished_at

video_fetch_candidates
  id, job_id, source_url_hash, normalized_url
  json_path, score, allowed_until, selected_at
  UNIQUE(job_id, normalized_url)

video_fetch_events
  id, job_id, sequence, event_kind, stage, detail_json, created_at
```

- URL 授权绑定 `job_id + normalized_url + expiry`，不再绑定进程内 map。
- worker 使用 lease 领取任务；启动恢复 expired lease。
- 每个阶段写 checkpoint，临时文件不作为状态来源。
- 下载完成后以内容 hash 创建不可变媒体对象；reload 后可从最近 durable checkpoint 继续。
- retention job 定期清理终态元数据，媒体引用仍沿用现有引用计数。

**具体解决**

plugin reload、进程退出和 HTTP status 轮询间隔都不会清空任务与授权候选。

**用户体验变化**

管理员 reload 插件后，用户的任务会继续或从安全阶段恢复；重复询问同一 job 仍能得到真实状态。

### 6.6 分阶段重试与视频发送幂等

**当前问题**

当前为避免重复视频，把所有视频发送失败、lease 过期和重启都直接死信。生产 12 个死信中 7 个只是 `DeadlineExceeded`，瞬时故障没有恢复机会。

**设计**

错误必须带结构化阶段：

| 分类 | 示例 | 行为 |
| --- | --- | --- |
| `safe_retry` | DNS 临时失败、429/5xx、下载前超时、ffmpeg worker 退出 | 指数退避并重试 |
| `permanent` | SSRF 拒绝、404、超大小、格式不支持、微信明确拒绝 | 直接失败 |
| `send_not_started` | 尚未调用微信 Host 就超时/退出 | 安全重试同一 Outbox |
| `send_ambiguous` | 请求已交给 Host，但 receipt 未知 | 不自动重发，进入人工/回执核对 |

- job、媒体对象和 Outbox 使用稳定 idempotency key。
- 下载/转码重试复用 job，不创建新的用户任务。
- 微信发送只有在 sender 明确报告 `not_started` 时自动重试。
- 若 Host 将来支持 client idempotency key 或 receipt 查询，可把 ambiguous 自动恢复；在此之前优先避免双发。
- dead letter 必须触发一次幂等的用户失败通知；通知本身使用 text Outbox。

**具体解决**

恢复真正可安全重试的 DeadlineExceeded，同时保留对不确定微信上传结果的防重复保护。

**用户体验变化**

网络抖动时任务更可能自动完成；确实不能安全重试时，用户会收到说明，不再只有沉默的死信。

### 6.7 持久化进度与最终失败通知

**当前问题**

用户只收到“派下去”，几十分钟没有任何状态；后台 completion 失败也未必产生可见结果。

**设计**

- durable job 创建成功立即发送一次接单确认，包含短 job 编号。
- 正常快速任务不刷屏；超过阈值才发送进度：默认 45 秒一次“正在下载/处理”，之后最多每 3 分钟一次，总计不超过 3 条。
- 阶段变化写 `video_fetch_events`，progress worker 根据 durable event 决定是否通知。
- 视频成功投递时，视频本身就是最终结果，不额外发送“已发送”文本。
- 永久失败或耗尽重试后，发送一次简短原因和可操作建议。
- `/hermes job <短编号>` 返回当前阶段、最近更新时间和失败原因。

**具体解决**

让慢任务、卡住任务和失败任务都具有可观察性，并避免依赖 Agent 自己记得回报。

**用户体验变化**

用户能区分“正在处理”“已经失败”和“视频已到达”，不需要几十分钟后反复追问。

### 6.8 混合路由与 Agent 前置过滤

**当前问题**

`social_mode=agent` 且 `sample_rate=1.0` 让 97.1% 消息进入 Chat。大量普通群聊启动完整模型，造成延迟、取消和上下文膨胀。

**设计**

推荐并最终默认使用 `social_mode: hybrid`：

1. 确定性过滤：机器人自己的消息、系统事件、撤回、空内容、明确发给他人的命令直接 ignore。
2. 明确触发：私聊、@Hermes、引用 Hermes、Owner 命令进入 `respond`。
3. 明确 capability 命令：如高置信视频 URL 指令进入对应 durable handler。
4. 其余群聊进入轻量 SocialDecider，只输出结构化 disposition。

原始 `agent` 模式继续保留作显式兼容选项，不直接删除。

**具体解决**

在昂贵 Agent 前截断绝大多数无需回复的群消息，避免它们占用 interactive lane。

**用户体验变化**

Hermes 不再对群里每句话都思考或插话；真正 @ 它时响应更快，也更少被其他聊天打断。

### 6.9 SocialDecider 使用结构化 Observe disposition

**当前问题**

当前依赖模型生成 `[silence]`、括号说明等文本，再由 Connector 猜测是否静默。Provider 输出差异会导致误回复或错误吞掉正文。

**设计**

SocialDecider 只允许返回：

```json
{
  "disposition": "ignore | observe | respond",
  "reason_code": "...",
  "confidence": 0.0
}
```

- `ignore`：只保留 Inbox 审计，不进入 Agent transcript。
- `observe`：写共享群 observation stream，不产生回复。
- `respond`：创建 interactive Run。
- schema 错误、超时和低置信度默认 `observe`，不能降级成完整 Agent 自由回复。
- Connector 的旧 silence 文本解析只作为旧版本兼容，不再是主协议。

**具体解决**

消除“文本看起来像静默指令”的脆弱协议，让路由决定可以统计、测试和审计。

**用户体验变化**

Hermes 的沉默和参与更稳定；不会把正常正文误吞，也不会把内部“保持沉默”说明发到群里。

### 6.10 共享观察与每用户交互会话分离

**当前问题**

整个群共用一条长交互 transcript。不同成员的问题和工具结果相互污染，输入达到 21K–23K tokens；新消息还会 supersede 正在处理的 ambient Run。

**设计**

一个群拆成两类上下文：

```text
group observation stream: group_id
interactive session:      group_id + speaker_id + thread_generation
```

- `observe` 消息按群共享，让 Hermes 知道近期群内发生了什么。
- 用户明确与 Hermes 交互时，使用该用户独立 session。
- 交互输入由“该用户短 transcript + 截止当前序号的群 observation 摘要 + 当前消息”组成。
- @ 多人或引用链需要群体语境时，可创建显式 shared thread，而不是默认把整个群永久混成一个 session。
- reset 默认只重置发起者的 interactive session；Owner 可显式重置 group observation。

**具体解决**

隔离不同成员的任务和工具调用，同时保留必要的群内共同背景。

**用户体验变化**

Hermes 更少把甲的问题回答给乙，也不会因为群里长期聊天而越来越慢；它仍能理解刚刚群里讨论的主题。

### 6.11 有界上下文 barrier

**当前问题**

共享 observation 异步写入时，interactive Turn 可能先启动，导致 transcript lag；直接拼接消息又触发 role alternation repair。

**设计**

- 每条群消息分配单调 `group_sequence`。
- interactive Run 固定 `observation_barrier_sequence = current_sequence - 1`。
- Context worker 在短时限内应用 barrier 前的 observation；超时则使用最近 durable summary，加一条结构化 lag marker，不阻塞整条消息。
- observation 只作为一个受控 context block 注入到当前 user turn，不追加伪造 assistant/user message。
- 配置总 token budget，建议初始值：用户 transcript 8K、群观察 4K、当前输入/工具预留 4K；超出时先摘要群观察，再淘汰最旧的用户回合。
- 摘要按 sequence 范围幂等保存，不能每次请求重新总结全部历史。
- 严格 role alternation 和 prompt caching 保持不变。

**具体解决**

减少 transcript lag 与 762 次 alternation repair，并给群上下文设置硬上限。

**用户体验变化**

回复更快、更聚焦；Hermes 看到的群内背景不会随机缺一段，也不会随着群存续时间无限变慢。

### 6.12 分离 ambient、interactive 与 durable-task admission

**当前问题**

新 ambient 消息会 supersede 老 ambient Run，interactive completion 和用户新消息还可能竞争同一 session guard。6504 个 ambient Run 中已有 694 个取消或失败。

**设计**

- `observe` 不再创建完整 Agent Run，只写 observation stream。
- SocialDecider 使用独立小并发池，允许合并尚未处理的连续普通消息。
- interactive lane 按每用户 session 串行，不被 ambient supersede。
- durable task lane 由 SQLite job lease 管理，不占 Hermes session guard。
- synthetic completion 只用于真正的开放式 delegation；视频 URL job 不使用它。
- admission 指标分别记录 `ignored/observed/responded/task_enqueued`。

**具体解决**

从根源减少 ambient Run 和取消竞态，并让长任务脱离聊天回合锁。

**用户体验变化**

群里有人继续说话不会取消用户已经明确交给 Hermes 的视频任务；@Hermes 的回复也不再排在一串背景 Agent 后面。

### 6.13 按角色与触发来源授权工具

**当前问题**

如果所有群消息都使用同一工具权限，ambient 或 synthetic completion 可能意外再次 delegate、发送媒体或执行有副作用的动作；简单把工具动态移出 prompt 又会破坏 session 和 prompt cache 稳定性。

**设计**

工具 schema 和 system prompt 在 session 内保持稳定，在 `pre_tool_call` 执行点校验不可伪造的上下文：

| 执行角色 | 视频 enqueue | 发送表情/媒体 | delegate | 只读 web |
| --- | --- | --- | --- | --- |
| private/addressed interactive | 允许 | 允许 | 允许 | 允许 |
| unaddressed interactive respond | 按配置 | 默认禁止 | 默认禁止 | 允许 |
| social observe/ignore | 禁止 | 禁止 | 禁止 | 禁止 |
| synthetic completion | 禁止 | 禁止 | 禁止 | 禁止 |
| isolated video selector | 禁止 | 禁止 | 禁止 | 禁止 |

授权依据来自 Golem/Hermes task-local binding，不接受模型参数中的 chat id、receiver 或 role 声明。

**已实现**

- Golem 在 active Run 完成 context/session/user/message 绑定校验后，直接读取
  `RunRequest.TriggerKind`；ambient 的 sticker search/materialize/select、video
  search/resolve/select 和 inline durable fetch 在 provider 调用、效果 staging 或 job
  创建前统一返回 403。
- Hermes Relay transport 将已认证 invocation frame 的 trigger kind 写入本地-only
  `SessionSource`，Gateway 再绑定到无环境变量 fallback 的 task-local ContextVar；该字段
  不进入 SessionSource wire/persistence serializer，模型参数和子进程环境无法伪造。
- 用户插件 `pre_tool_call` 在 ambient 回合阻止 `delegate_task` 与全部
  `golem_sticker_*` / `golem_video_*` 工具，保留文本回复和只读 web。
- ambient 最终文本同时移除 `MEDIA:`、Markdown/HTML 图片和本地媒体路径，堵住不经过
  capability endpoint 的附件语法旁路。
- explicit/control、私聊和没有 Relay trigger binding 的传统 Hermes surface 保持原能力；
  synthetic completion 原有视频/递归任务限制保持不变。

**具体解决**

避免群聊背景消息和 completion 回合产生递归任务或错误发送，同时遵守 Hermes 的固定 toolset 约束。

**用户体验变化**

明确 @Hermes 时功能完整；没有叫它时，它不会因为听到一个链接就擅自下载或往群里发媒体。

## 7. 数据与 API 变更

### 7.1 Producer API

新增或升级：

```text
POST /capabilities/v2/producers/acquire
POST /capabilities/v2/producers/heartbeat
POST /capabilities/v2/producers/reconcile
```

`acquire` 返回持久化 `owner_id + generation + lease_until`。只有当前 owner generation 能 register 新 ticket 或 reconcile。child 进程不持有 acquire credential。

### 7.2 Video Job API

```text
POST /capabilities/v2/video-jobs
GET  /capabilities/v2/video-jobs/{job_id}
POST /capabilities/v2/video-jobs/{job_id}/cancel
```

创建请求不接收微信目标参数；目标从 active Run binding 反查。请求包含稳定 `invocation_id`，相同 binding 与参数幂等返回原 job，参数不同返回 409。

状态响应示例：

```json
{
  "job_id": "vfj_...",
  "state": "processing",
  "stage": "transcoding",
  "accepted": true,
  "outbox_id": null,
  "updated_at": "...",
  "failure": null
}
```

旧 `/async-delivery/videos/inspect` 和 `/send-url` 在迁移期保留，但内部转调 durable job service；不再建立独立内存 job。

### 7.3 Delivery receipt 回写

Dispatcher 在同一事务中：

- `MarkOutboxSent`；
- 更新关联 `video_fetch_jobs.state=delivered`；
- 写 `video_fetch_events(delivered)`。

进入 `dead_letter/ambiguous` 时同样更新 job，并幂等创建失败通知。任何状态接口都从 SQLite 读取，不依赖 Python 本地 completion 标记。

## 8. 配置

新配置放入 Hermes `config.yaml` 和 Golem 现有 TOML 配置结构，不新增非 secret 环境变量。

建议初始配置：

```yaml
routing:
  social_mode: hybrid
  group_interactive_sessions: per_user
  social_decider:
    timeout_seconds: 3
    min_respond_confidence: 0.72
  context_budget:
    interactive_tokens: 8000
    observation_tokens: 4000
    reserve_tokens: 4000

video_jobs:
  immediate_ack: true
  first_progress_after_seconds: 45
  progress_interval_seconds: 180
  max_progress_messages: 3
  selector_max_calls: 1
```

具体字段接入哪个现有配置对象在实施阶段确定，但不能通过新环境变量绕开配置加载和 profile 作用域。

## 9. 迁移与发布顺序

### 阶段 0：锁定现有行为

- 为当前未提交 URL 安全、下载/转码和 follow-up session rebind 补齐行为测试。
- 测试只断言协议和结果，不断言源码字符串或 monkey-patch 实现细节。
- 保存生产基线指标，确保后续可比较。

### 阶段 1：先修 ticket 自我 abandon 与假 delivered

- 引入 root-only producer owner/reconcile。
- 修正 completion terminal disposition。
- 先 shadow 记录新旧判断，再启用接管。

这是最高优先级，因为它直接造成当前“派下去后消失”。

### 阶段 2：持久化 video job

- 建表和 migration。
- 内存 API 改为 durable service facade。
- 增加 worker lease/recovery。
- 旧 endpoint 继续兼容 Python 工具。

### 阶段 3：切换确定性视频路径和进度 UX

- 上线 enqueue API、URL command router 和 isolated selector。
- 开启接单/进度/失败通知。
- 完成 video retry classification。

### 阶段 4：混合路由和上下文拆分

- SocialDecider 先只记录 disposition，不改变线上路由。
- 比较 shadow 结果后，小比例启用 `ignore/observe`。
- 再启用 per-user interactive session 和 bounded barrier。
- 保留快速回退到旧 `agent` 模式的配置开关。

### 阶段 5：清理兼容层

- 达到观察期指标后移除进程内 video job/url map。
- 旧 silence 文本协议和 v1 endpoint 经过一个完整版本弃用期后再删除。

Golem plugin 与 Hermes Gateway 必须按兼容顺序滚动：先部署兼容新旧协议的 Golem，再部署 Hermes；回滚顺序相反。plugin reload 不应要求停止 Golem Host。

## 10. 测试要求

### 10.1 Golem

- child plugin load 不得调用 reconcile；
- 同 Gateway boot 的 profile 使用相同 owner generation；
- 旧 owner 不能 abandon 当前 owner ticket；
- plugin reload 后 job、candidate、lease 和 status 保留；
- URL 唯一候选不调用 selector；多候选最多调用一次；
- SSRF、DNS rebinding、跨源重定向和大小限制继续生效；
- 每个 checkpoint 崩溃后可恢复且不重复创建 Outbox；
- `DeadlineExceeded/not_started` 重试，`send_ambiguous` 不重发；
- 只有 `MarkOutboxSent` 后 job 才是 delivered；
- progress/failure 通知幂等且受条数限制；
- clear URL command 只在 private/addressed 条件命中。

执行：

```text
go test -timeout 60s ./...
```

### 10.2 Hermes Agent 与 Python 插件

- 保留当前 sender/session follow-up rebind 的 409 回归测试；
- 根进程与 child 的 producer role 区分；
- synthetic completion 不得调用 action tools；
- fixed tool schema 下按 task-local role 拒绝工具；
- SocialDecider schema 错误默认 observe；
- per-user session 不串 speaker；
- observation barrier 保序、超时可降级且不破坏 role alternation；
- prompt caching 输入前缀保持稳定；
- `/new`、`/reset`、`/stop` 只作用于正确 session。

Hermes Python 测试必须通过仓库脚本运行：

```text
scripts/run_tests.sh <相关测试路径>
```

不能直接运行 pytest。

### 10.3 故障注入

必须在以下位置强制 kill/reload 并验证恢复：

- inspect 前后；
- 下载中；
- ffmpeg 中；
- 媒体对象已提交但 Outbox 未创建；
- Outbox 已创建但未 lease；
- sender 调用前；
- sender 已调用但 receipt 丢失；
- Gateway root 重启与 child plugin discovery 并发。

## 11. 验收指标

上线后至少持续观察一个完整高峰周期：

| 指标 | 验收目标 |
| --- | ---: |
| child/plugin reload 导致 ticket abandoned | 0 |
| 明确视频指令 durable 接单 P95 | < 2 秒 |
| 唯一 URL/JSON 候选模型调用数 | 0 |
| 歧义 URL 候选模型调用数 | <= 1 |
| reload 后可查询/恢复的视频 job | 100% |
| `DeadlineExceeded` 被无条件视频死信 | 0 |
| 假 delivered（无 receipt） | 0 |
| 普通群消息进入完整 Chat 比例 | 初期 < 30%，稳定后 < 15% |
| role alternation repair | 接近 0，且不能由新注入产生 |
| 群交互 P95 输入上下文 | <= 16K tokens |
| 明确任务被 ambient supersede | 0 |

如果 `respond` 漏判率、用户明确召唤延迟或上下文正确性变差，优先回退 routing/context 开关；durable job、诚实状态和 producer owner 修复不回退到进程内实现。

## 12. 实施完成后的整体体验

完成 V3 后，用户发送：

```text
@Hermes 把这个视频发给我 https://example.com/api
```

预期体验是：

1. 很快收到“已接单，任务 vfj_xxxx，正在解析视频”。这表示任务已落 SQLite，不是口头承诺。
2. 常见直链或单候选 JSON 不再调用后台子 Agent，直接进入下载和转码。
3. 处理较慢时收到有限的阶段进度；群里继续聊天不会取消任务。
4. plugin reload 后任务仍可继续，`/hermes job vfj_xxxx` 能查到同一状态。
5. 视频真正发送到群里或私聊后才算 delivered。
6. 如果失败，用户会收到明确原因；如果微信发送结果不确定，系统不会冒险自动双发。

与此同时，Hermes 对普通群聊以观察为主，不再让每句话都跑完整 Agent。用户明确 @ 它时，交互上下文更短、更干净，并且不会被其他群成员的任务串扰。
