# Hermes 插件架构设计

状态：设计已确认
目标版本：Hermes 1.0
性质：全新 Golem 插件，不继承 ai 插件内部实现
部署形态：单机

## 1. 已确认的架构约束

1. 不修改 Golem Host、SDK 或共享 Proto。
2. Hermes 是 plugins/hermes 下的全新插件，和 plugins/ai 并存，独立配置、存储和生命周期。
3. 允许修改或替换现有 Python Hermes Agent，使 Agent 原生支持异步事件、取消、工具代理和检查点。
4. 首期只实现内建能力；扩展系统最后开发，但核心接口从第一天保持可替换。
5. 扩展仅要求支持 Go，不承诺 Python 或 JavaScript 第三方扩展。
6. 单机需要支撑上百人的自然群聊。
7. 普通群聊是否调用决策 Agent 由配置控制，以适应 Coding Plan 的 Token 余量。
8. 用户体验目标是：快速确认、自然进度、可取消或补充、最终可靠发送。
9. 普通闲聊必须走高优先级快速通道，不能排在长任务之后。
10. 发送语义选择至少一次：宁可在极端故障窗口重复，也不能静默丢失回复。

## 2. 设计目标

### 2.1 用户体验

- 私聊、@、引用等明确交互立即进入交互通道。
- 健康模型服务下，普通回复第一条消息 P95 小于 3 秒。
- 明确请求入队或需要等待时，300 毫秒内能够生成确认事件；是否实际发送确认由拥塞预测决定。
- 长任务不阻塞同一会话后续闲聊。
- 取消和补充使用高优先级控制通道，不排在普通消息之后。
- Agent 自然决定普通群聊是否参与，不依赖固定概率作为唯一策略。
- 不把逐 Token 输出直接刷屏到微信；内部可以流式，外部以自然消息和有限进度呈现。

### 2.2 可靠性

- OnEvent 返回前完成最小持久化，插件崩溃后可重放。
- 输入按消息 ID 去重。
- 同会话尽最大可能恢复真实顺序，不同会话并行。
- 所有输出先写持久化 Outbox，再调用 Golem 发送能力。
- Worker、模型、工具、发送和存储故障相互隔离。
- 取消从会话控制消息传播到 Run、Agent、模型调用和工具调用。
- 插件卸载时停止接收、取消运行、回收 Worker，并保留未完成工作供下次恢复。
- 不使用无限 goroutine、无限队列或无限内存会话缓存。

### 2.3 扩展性

- 模型、Agent、路由器、工具、记忆、存储、输入、输出、渲染和观测均通过明确接口连接。
- 核心只依赖接口和领域事件，不依赖 MCP、OpenAI 消息格式或具体 Python 类。
- Go 扩展通过编译期模块注册实现，避免 Go plugin 动态 ABI 的版本脆弱性。
- 配置按模块命名空间组织，新增模块不修改核心配置结构。

## 3. 非目标与不可突破的边界

### 3.1 不修改宿主带来的边界

Host 当前会并发分发事件，插件接口仍是同步 OnEvent。Hermes 通过“同步持久化接受、异步处理”适配这一边界，但不能改变宿主自身的调度方式。

Message 已包含稳定的消息 ID，因此文本、图片和表情事件可以精确去重。宿主没有为所有非消息事件提供统一事件 ID，非消息事件使用内容指纹兜底。

Host 不提供输入确认、事件重投协议，因此 Hermes 只能保证已进入 OnEvent 且成功写入 Inbox 的事件可恢复。

SDK 的发送请求没有调用方幂等键。发送成功但响应丢失时，Hermes 无法知道微信是否已经收到。因此只能在以下两种语义中选择：

- 至少一次：不丢消息，极端情况下可能重复。
- 至多一次：不重复，极端情况下可能丢消息。

本设计按用户要求选择至少一次。

### 3.2 单机边界

- 首期不引入 Redis、NATS、Kafka 或 PostgreSQL。
- SQLite 是系统事实来源，WAL 模式配合独立写入器。
- 不承诺多实例并发消费或跨机器容灾。
- 单机进程级崩溃可以恢复；整机磁盘损坏不在本设计保证范围内。

## 4. 旧 ai 插件的缺陷与 Hermes 对应方案

| 旧缺陷 | 用户可见结果 | Hermes 方案 |
| --- | --- | --- |
| 用 goroutine 尾链模拟同会话队列 | 队列无界、难以取消、卸载不能收敛 | 持久化 Inbox、有限 Session Actor、统一调度器 |
| Host 并发回调到达顺序不稳定 | 连续消息可能前后颠倒 | Message ID 去重，按时间和消息 ID 的短窗口重排 |
| 长任务持有同会话顺序锁 | 后续闲聊一直等待 | Interactive、Job、Control 三条隔离通道 |
| 取消只修改内存状态 | Hermes 和工具仍继续执行 | 根 Context 到 Run、模型和工具的协作式取消 |
| 补充要等旧任务结束再整轮重跑 | 响应慢、浪费 Token | 立即取消当前 Revision，从最新检查点创建新 Revision |
| Python worker 同步读取请求并只返回最终结果 | 无进度、无心跳、无法安全中断 | 全双工版本化 Agent 协议和事件流 |
| Worker 缓存多个会话，超时杀整个进程 | 一个任务影响多个会话 | Worker 无会话所有权、一次只运行一个 Run |
| Go 上下文和 Python Agent 会话状态双写 | 上下文漂移、重启后行为不同 | Event Store 为唯一事实来源，Worker 每次接收不可变快照 |
| Agent 直接通过 MCP 发送微信 | 副作用绕过事务，取消与补充难以兜住 | Agent 只提交 ReplyProposal，Go Outbox 统一发送 |
| 使用 REPLIED、NO_REPLY 等提示词魔法值 | 模型偏差会导致漏发或重复 | 结构化 AgentEvent 和 RouteDecision |
| MCP 工具由静态 switch 注册 | 新能力必须修改核心 | 类型化 ToolRegistry 和模块注册 |
| 路由信息是可变的最后一条消息 | 可能出现权限时序问题 | 每个 Run 持有不可变 Principal 和 ChannelBinding |
| 配置快照包含共享 Map 和指针 | 并发读写存在竞态风险 | 完整深拷贝后用 atomic.Pointer 发布不可变快照 |
| 数据库操作散落在业务路径 | 锁竞争、事务边界不清楚 | Store Port、单写入器、显式 Unit of Work |
| 发件箱发送后整体删除 | 部分成功、崩溃重发难以判断 | 每个 Outbox Item 独立状态、尝试记录和发送回执 |
| 图片、历史、记忆、策略全部堆在主结构体 | 修改一处容易影响整个插件 | 领域模块和端口分层 |
| 本地文件和网络图片由 Agent 任意指定 | SSRF 和任意文件读取风险 | 工具级 Capability、路径根目录和网络策略 |
| 后台 goroutine 不受生命周期管理 | 重载后旧任务仍可能运行 | Supervisor 根 Context、Drain 和 WaitGroup |

## 5. 总体架构

    Golem Host
        |
        | synchronous OnEvent
        v
    Golem Adapter
        |
        | durable accept
        v
    Inbox Journal -----> Recovery Scanner
        |
        v
    Reorder + Deduplicate
        |
        v
    Session Directory
        |
        +-------------------+-------------------+
        |                   |                   |
        v                   v                   v
    Control Lane      Interactive Lane       Job Lane
    highest priority  low latency            long-running
        |                   |                   |
        +-------------------+-------------------+
                            |
                            v
                    Fair Run Scheduler
                            |
              +-------------+-------------+
              |                           |
              v                           v
        Router Pool                  Agent Worker Pool
              |                           |
              +-------------+-------------+
                            |
                            v
                     Go Tool Broker
                            |
                +-----------+-----------+
                |                       |
                v                       v
          Read-only Tools         Effect Proposals
                                        |
                                        v
                                Transactional Outbox
                                        |
                                        v
                                Golem Output Adapter

核心原则：

- Host 回调只负责可靠接受，不负责 Agent 执行。
- Session Actor 只负责状态变更和提交顺序，不执行慢 I/O。
- 所有慢工作由 Scheduler 派发到有界 Worker Pool。
- Job 可以和 Interactive Run 并行，但每个会话最多一个正在提交回复的 Interactive Run。
- Agent 不直接拥有微信 Receiver，也不能绕过 Outbox。

## 6. 组件职责

### 6.1 Golem Adapter

职责：

- 实现 Plugin、Lifecycle、EventPlugin、CommandPlugin。
- 把 SDK Message 转换为 Hermes Envelope。
- 保存不可变 ChannelBinding，包括会话、接收人、发言人和权限主体。
- 屏蔽 Golem Proto 类型，核心领域不引用 sdk/message。
- OnEvent 中完成去重键计算和 Inbox 插入。
- 插入成功后非阻塞唤醒 Dispatcher，并立即返回。

OnEvent 不做以下工作：

- 不调用模型。
- 不下载图片。
- 不等待同会话前一个事件。
- 不发送微信。
- 不执行策略模型或 Agent。

### 6.2 Inbox Journal

Inbox 是异步边界，也是崩溃恢复入口。

消息去重键优先使用：

    wechat/message/{message.id}

当 ID 为 0 时使用：

    sha256(topic, session, speaker, timestamp, type, content, media_md5, raw)

写入与会话序号分配在同一事务完成。成功提交后 OnEvent 才返回。

写入完成后的内存通知只是优化；即使通知丢失，Recovery Scanner 也会扫描数据库中的 accepted 事件。

### 6.3 Reorder Buffer

Host 可能同时启动多个事件分发 goroutine，因此到达插件的顺序不一定等于微信顺序。

每个会话使用一个短重排窗口：

- 默认 120 毫秒。
- 排序键为 timestamp、message_id、accept_sequence。
- 明确命令和控制消息可以跳过等待，直接进入 Control Lane。
- 超过窗口后不再等待可能缺失的事件，避免为了严格顺序牺牲实时性。

这提供“尽最大可能有序”，而不是虚假的绝对顺序承诺。

### 6.4 Session Actor

每个活跃会话有一个轻量 Actor，Actor 只拥有：

- 当前会话版本。
- 最近已提交 Turn 的引用。
- 交互 Run 状态。
- 活跃 Job 和 Revision 引用。
- 三条逻辑 Mailbox。
- 会话配置快照版本。

Actor 不保存完整消息正文，正文从 Store 按需读取。

Actor 空闲后默认 15 分钟回收。再次出现消息时从数据库重建，因此活跃会话数不会永久增长。

### 6.5 三条执行通道

#### Control Lane

优先级最高，处理：

- 取消。
- 补充。
- 进度查询。
- 停止发送。
- Owner 管理指令。

先执行确定性规则，无法确定时调用轻量 Control Router。Control Router 有独立并发额度，绝不与长任务共享 Worker。

#### Interactive Lane

处理：

- 私聊。
- @ 或引用。
- 普通短对话。
- 普通群聊中被 Social Router 选中的参与。

同一会话最多一个正在生成回复的 Interactive Run，避免机器人回复自身已经过时的上下文。Interactive Lane 不等待 Job Lane。

#### Job Lane

处理：

- 搜索。
- 图片理解。
- 文件解析。
- 报告生成。
- 渲染。
- 其他多步骤工具任务。

Job 使用发起时的会话快照和独立 Revision。默认每个会话一个活跃 Job，新的独立 Job 排队；补充会取消旧 Revision 并启动新 Revision。

### 6.6 Fair Run Scheduler

Scheduler 使用分级优先队列和会话公平调度：

1. Control。
2. 明确私聊、@、引用。
3. 普通 Interactive。
4. Social Router。
5. Job。
6. Summary、索引和清理。

同一繁忙群不能占满所有执行槽。每个会话通过 Deficit Round Robin 获得配额。

各资源池独立设置并发上限：

- Router Pool。
- Interactive Agent Pool。
- Job Agent Pool。
- Tool Pool。
- Output Pool。

Job 饱和时不会占用 Interactive 的保留容量。

## 7. 普通闲聊由谁决定

语义上由 Agent 决定；调度上由 Hermes 内核决定。

明确交互不经过 Social Router：

- 私聊直接进入 Interactive。
- @ 机器人直接进入 Interactive。
- 引用机器人直接进入 Interactive。

只有普通群聊消息需要 Social Router。支持五种配置模式：

| 模式 | 行为 |
| --- | --- |
| observe | 只记录，不调用决策模型 |
| rules | 仅使用本地规则 |
| mentions | 私聊照常处理；群聊仅 @机器人或引用机器人时调用 Hermes，@其他人保持观察 |
| hybrid | 本地规则过滤后调用决策 Agent |
| agent | 每条合格普通群消息调用决策 Agent |

另提供 sample_rate，使 Coding Plan 强度较高时降低普通消息调用比例。

Social Router 只输出结构化结果：

    OBSERVE
    CHAT
    JOB

它不生成最终回复，也不占用 Interactive Agent Worker。

超时降级：

- 普通群聊：OBSERVE。
- 私聊、@、引用：不走 Social Router，因此不受影响。
- 控制意图：由本地规则继续处理，语义分类失败时作为普通新消息。

普通群消息有 freshness_deadline。默认超过 5 秒仍未开始执行则只进入历史，不再突然回复过时话题。

## 8. 全异步 Agent Runtime

### 8.1 运行时原则

- Python Worker 是执行器，不是会话数据库。
- Worker 一次只执行一个 Run。
- Run 上下文由 Go 从 Event Store 构建不可变快照。
- Worker 完成后可以复用，但不能保留不可恢复的会话真相。
- Worker 崩溃只影响当前 Run。
- Job 和 Interactive 使用独立 Worker Pool。

### 8.2 Agent Engine 接口

Go 核心只依赖以下概念：

    type AgentEngine interface {
        Start(ctx context.Context, request RunRequest) (RunStream, error)
        Health(ctx context.Context) Health
        Close(ctx context.Context) error
    }

    type RunStream interface {
        Recv(ctx context.Context) (AgentEvent, error)
        Send(ctx context.Context, command AgentCommand) error
        Close() error
    }

RunRequest 包含：

- run_id。
- session_id。
- principal。
- lane。
- base_session_version。
- conversation_snapshot。
- system_profile。
- tool_specs。
- deadline。
- checkpoint。
- revision。

### 8.3 Plugin 私有协议

不修改共享 Proto。Hermes 在自身目录维护版本化私有协议。

首期使用长度前缀 JSON Frame，避免引入 Python gRPC 运行时依赖：

- 4 字节大端长度。
- UTF-8 JSON Payload。
- 单帧默认上限 8 MiB。
- stdout 只传协议，stderr 只传日志。

每个 Frame 包含：

    version
    kind
    run_id
    sequence
    correlation_id
    timestamp
    payload

Go 到 Python 的命令：

- hello。
- start_run。
- cancel_run。
- revise_run。
- tool_result。
- ping。
- drain。
- shutdown。

Python 到 Go 的事件：

- ready。
- run_accepted。
- thinking_delta。
- reply_proposed。
- tool_call_requested。
- tool_call_cancelled。
- progress_proposed。
- checkpoint_saved。
- run_completed。
- run_failed。
- pong。

thinking_delta 只用于内部观测，不直接发送到微信。

### 8.4 Python Agent 必须进行的改造

新的 Agent 入口必须是异步生成器或事件回调，而不是同步 run_conversation：

    async def run(request, emit, tools, cancellation):
        ...

强制要求：

- HTTP 兼容模式的模型调用接受取消信号和绝对 Deadline；Relay 模式只接受生命周期/Owner 取消，由 Hermes `agent.gateway_timeout` 负责无活动超时，避免 Connector 墙钟超时遗失仍在运行的 Turn。
- 工具调用通过 Go Tool Broker，不在 Python 内直接执行微信副作用。
- 每个工具调用有 invocation_id。
- 多步骤任务在安全点产生 Checkpoint。
- 收到 cancel_run 后停止产生新副作用，取消未完成模型和工具调用。
- 无法协作取消的第三方同步代码必须运行在可终止的子进程中。

取消流程：

1. Go 将 Run 标为 cancel_requested。
2. Go 发送 cancel_run。
3. Python 取消模型请求和未完成工具调用。
4. 超过 cancel_grace_period 仍未结束，Go 终止该 Worker。
5. 只有该 Run 失败，其他 Worker 和会话不受影响。

### 8.5 心跳与 Worker 监管

- Worker 启动必须协商协议版本和能力。
- 空闲时每 5 秒 Ping。
- 运行时 AgentEvent 本身可作为活性证据。
- 15 秒无事件且无已知阻塞工具时触发健康探测。
- 心跳失败先请求 Dump，再终止 Worker。
- Worker 连续崩溃使用指数退避和 Circuit Breaker。
- Interactive Pool 至少保留一个健康槽位，不被 Job 占用。

### 8.6 Hermes Gateway relay adapter

默认生产接入使用 Hermes Agent 官方 generic relay adapter，而不是把 Hermes 降级成 OpenAI-compatible HTTP completion：

```text
Hermes Gateway --WebSocket /relay--> Golem Hermes connector
               <-- descriptor/inbound/outbound_result -->
```

接入遵循官方 experimental contract v1：Gateway 主动拨号，同一条已鉴权 WebSocket 承载 `hello`、`descriptor`、`inbound`、`outbound`、`outbound_result` 和 interrupt。Golem connector 把 Gateway 的 outbound action 转换为结构化 output proposal；它返回的 success 只表示接受提案，微信 SDK 发送仍必须经过 Transactional Outbox。

边界保持不变：

- Golem Event 先进入 Durable Inbox，Gateway 不参与 OnEvent。
- Gateway 不接触真实 Receiver。
- Gateway 不直接调用微信发送能力。
- relay 平台禁用 Gateway MCP 和本地副作用工具，能力授权仍归 Go Broker。
- contract v1 没有 turn-completed frame，因此不能使用会被 Gateway 吞掉的 `SILENT`/`NO_REPLY`。群聊使用专用内部观察标记：Hermes 对 ambient 消息结合共享群上下文自主决定；如果 addressed 消息也返回该标记，Connector 同样产生 `RunCompleted` 且不创建 output proposal/Outbox，保证 Session 活性；协议扩展显式完成事件后再迁移。
- contract v1 尚未定义 media outbound action。0.5.0 使用受控的本地 Capability API 暂存 image/emoji EffectProposal，并继续以标准 final send 作为 Run 完成边界；官方协议增加 media action 后可替换桥接层，不改变 Outbox 所有权。

HTTP chat-completions 只保留为显式兼容 adapter，不是架构中心。

## 9. Tool Broker

Agent 不再连接插件自己的 MCP HTTP Server。所有工具通过 Go Tool Broker 暴露。

工具接口：

    type Tool interface {
        Spec() ToolSpec
        Invoke(ctx context.Context, call ToolCall) (ToolResult, error)
    }

ToolSpec 包含：

- 名称和版本。
- 输入与输出 Schema。
- 所需 Capability。
- 是否只读。
- 是否有副作用。
- 默认 Timeout。
- 并发限制。
- 重试策略。
- 是否支持 Checkpoint。

工具分为两类：

### 9.1 只读工具

例如：

- 读取会话历史。
- 搜索记忆。
- 列出图片。
- 分析图片。
- 搜索网络。

只读工具可以在 Run 中立即执行，结果作为 tool_result 返回。

### 9.2 副作用提案

例如：

- 回复文本。
- 发送图片。
- 发送渲染结果。
- 更新持久策略。

Agent 不能直接执行这些副作用，只能产生 EffectProposal。Go 校验权限后写入暂存区。

最终回复不要求 Agent 调用“发送微信”工具。Agent 直接发出 reply_proposed，Go 根据 Run 状态将其转换为 Outbox Item。

这样消除以下脆弱约定：

- 必须调用 reply_to_wechat。
- 最终只能返回 REPLIED。
- 没回复时返回 NO_REPLY。
- 工具已发出但 final response 又被兜底发送。

### 9.3 Capability 安全

每个 Run 获得最小权限集合，例如：

    history.read.current_session
    image.read.current_session
    memory.read.current_session
    memory.write.current_session
    output.propose.current_session
    policy.write.owner_only

Agent 只看到不透明 session_id，不看到真实 Receiver。ChannelBinding 只存在 Go 内核。

网络工具使用域名、协议和私网访问策略；文件工具只能访问配置的 Workspace Root，禁止任意绝对路径。

### 9.4 Hermes 用户插件 Capability 桥

官方 Relay contract v1 没有 tool-call frame。为了保留 Hermes 原生工具选择能力，同时不恢复 MCP 微信发送工具，0.5.0 采用一个窄化桥接：

```text
Hermes user plugin
  -> search(query, limit)
  -> opaque candidate IDs
  -> select(candidate ID)
  -> stage EffectProposal on current relayRun
  -> final Relay send
  -> CommitRunSuccess(text + emoji drafts)
  -> Transactional Outbox
```

桥接端点与 Relay listener 同进程，只接受独立 Bearer token。Hermes 通过 task-local session ContextVar 提供 platform/chat/session/user/message，Go 必须再次匹配当前活动 Run；message ID 防止旧 Turn 的延迟工具调用误绑定到同一聊天的下一 Run。这些客户端字段不是授权来源。模型参数不包含 Receiver、wxId、chat ID、URL 或路径。

Provider 负责把可配置 HTTP/JSON API 映射成候选，SearchService 用随机 ID 将候选绑定到 Run/chat 并设置 TTL。Select 才下载媒体，执行 HTTPS、CDN allowlist、Public IP、重定向、大小和 magic 校验，生成自包含 EmojiOutput.Data。Provider URL 不进入模型或 Outbox，第三方 URL 在提交后失效不会破坏重试。

Agent 可以选择文字、表情、二者或 ambient 观察。纯表情通过版本化内部 effect-only token 完成；只有已暂存 effect 时才接受。观察完成会清空暂存 effect。文字与 effect 按确定顺序进入同一个 Run 成功事务，不存在 Hermes 直接发送微信的第二条链路。

## 10. 状态模型

### 10.1 Turn 状态机

    accepted
      -> ordered
      -> routed
          -> observed
          -> queued_interactive
          -> queued_job
      -> running
      -> committing
      -> completed

异常终态：

    expired
    cancelled
    failed
    dead_letter

状态转换与对应 Run 或 Outbox 写入必须在同一数据库事务内完成。

### 10.2 Run 状态机

    queued
      -> leased
      -> running
          -> succeeded
          -> retry_wait
          -> cancel_requested -> cancelled
          -> failed
          -> orphaned

租约防止插件重启后 Run 永久停留在 running。启动恢复时，过期 Lease 进入 retry_wait 或从 Checkpoint 恢复。

### 10.3 Job 与 Revision

Job 是用户意图；Revision 是一次具体执行。

补充流程：

1. Control Lane 识别补充。
2. 持久化补充消息并递增 Job Revision。
3. 当前 Revision 进入 cancel_requested。
4. 丢弃尚未提交的 EffectProposal。
5. 从最近安全 Checkpoint 和最新会话快照创建新 Revision。
6. 新 Revision 进入 Job Pool。

旧 Revision 即使迟到返回，提交时也会因 Revision 不匹配被拒绝。

### 10.4 会话版本

每次提交输入或可见输出时递增 session_version。

Interactive Run 记录 base_session_version。提交前 Actor 检查：

- Run 是否仍是当前 Interactive Run。
- 目标 Turn 是否未被取消。
- 输出是否超过 Staleness Deadline。

Job 输出按 job_id 和 revision 提交，不要求会话在任务期间保持静止。

## 11. 持久化设计

SQLite 使用 WAL、busy_timeout、foreign_keys 和版本化 Migration。

建议核心表：

| 表 | 用途 |
| --- | --- |
| inbox_events | 原始输入、去重键、接受状态 |
| sessions | 会话版本和投影元数据 |
| turns | 路由结果和 Turn 状态 |
| messages | 规范化可见消息 |
| runs | Run 状态、Lease、Deadline |
| run_events | Agent 事件流和调试游标 |
| jobs | 长任务主状态 |
| job_revisions | 每次修订和 Checkpoint |
| tool_invocations | 工具调用、结果和错误 |
| effect_proposals | 尚未提交的副作用 |
| outbox | 待发送的最终消息或进度 |
| delivery_attempts | 每次发送尝试和响应 |
| media_objects | 图片等媒体元数据 |
| memories | 长期记忆 |
| summaries | 会话摘要 |
| dead_letters | 需要人工处理的永久失败 |

写入策略：

- 一个 Store Writer 串行提交短事务。
- OnEvent 使用高优先级持久化请求。
- 读操作使用独立只读连接池。
- 大媒体写入文件对象目录，SQLite 只保存内容寻址引用。
- 数据库中不长期保存重复大 Blob。
- 所有 Schema Migration 可重复执行并有版本号。

Event Store 是唯一事实来源；内存中的 Actor、缓存和索引都可以重建。

## 12. Transactional Outbox 与至少一次发送

### 12.1 提交

Agent 产生 reply_proposed 后，Session Actor 在一个事务中：

1. 校验 Run、Revision、权限和会话版本。
2. 将 ReplyProposal 转换为一个或多个 Outbox Item。
3. 更新 Run 和 Turn 状态。
4. 提交事务。

只有事务成功后 Output Dispatcher 才能发送。

### 12.2 发送

Outbox Item 状态：

    pending
      -> leased
      -> sent
      -> retry_wait
      -> dead_letter

发送成功时保存 SDK 返回的 new_id 和 create_time。

失败分类：

- 普通消息明确临时失败：指数退避并重试。
- 限流：遵循 Retry Budget 和会话发送间隔。
- 明确永久失败：进入 dead_letter 并告警，不删除。
- 普通消息结果不确定：最多尝试 `ambiguous_max_attempts` 次，因此可能重复。
- 视频发送失败：无论明确失败还是结果不确定，立即进入 dead_letter；微信视频发送没有幂等键，重试会造成可见重复。
- `retry_wait` 和历史失败的后台重试不占用会话队首；后续消息继续发送。

Outbox Item 永不因为进程重启而被静默删除。

### 12.3 顺序和节流

每个 Receiver 有一个逻辑 Output Actor：

- 首次发送按 outbox_sequence 串行；失败后的后台补偿不阻塞后续首次发送。
- 使用 Timer 调度间隔，不用 time.Sleep 占住业务锁。
- 文本、图片、进度和渲染结果共享同一 Receiver 顺序。
- 不同 Receiver 并行。

### 12.4 SDK 阻塞隔离

SDK Ability 使用同步调用。Golem Adapter 优先从注入的 message.Client 取得底层 gRPC Client，并使用带 Deadline 的流调用。

若注入的是其他 Ability 实现，调用放入有界隔离槽。槽耗尽时不继续创建 goroutine，Outbox 保持 pending 并触发健康告警。

## 13. 上下文与记忆

### 13.1 短期上下文

- 从 messages 投影构建，不在 Go 和 Python 各维护一份独立真相。
- 每次 Run 使用明确的 Context Budget。
- 最近消息、滚动摘要、相关记忆和媒体引用分别占用预算。
- Speaker、Quote、Mention 和事件时间使用结构化字段，不拼成不可解析的大字符串。

### 13.2 长期记忆

首期使用 SQLite FTS5 和结构化标签，不只使用 LIKE：

- 事实正文。
- 主体。
- 来源消息。
- 置信度。
- 生效与失效时间。
- 创建者。
- 可见范围。

记忆写入是 EffectProposal，必须经过策略校验。用户纠正事实时保留版本和来源，不直接覆盖审计记录。

向量检索保留 MemoryIndex 接口，首期不是强依赖。

### 13.3 摘要

摘要使用最低优先级后台通道，不进入 Interactive Worker Pool。

摘要基于 through_message_id 增量生成，事务提交时检查版本，防止旧摘要覆盖新摘要。

## 14. 图片和媒体

- 图片事件先记录元数据和可恢复的 SDK 下载凭据，不自动调用视觉模型。
- OnEvent 不下载图片；只有 worker 获得 Run lease 后，Golem 媒体适配器才调用 `message.Ability.Download`。
- 输入媒体解析由执行阶段适配器负责；视觉分析等主动媒体操作仍通过有大小、类型、Deadline 和并发限制的 Tool。
- 原始文件按 SHA-256 内容寻址保存。
- 视觉结果缓存键包含内容哈希、模型、提示词版本和工具版本。
- Agent 通过不透明 media_id 引用图片。
- 视觉 Agent 返回结构化观察结果，再由对话 Agent结合真实问题组织回答。
- 图片发送必须进入 Outbox，不接受 Agent 任意本地路径。

## 15. 配置模型

配置读取后完成验证和深拷贝，再通过 atomic.Pointer 发布不可变 ConfigSnapshot。运行中的 Run 记录 config_version，不受半途配置修改影响。

建议配置示例：

    [hermes.config]
    data_dir = "data/hermes"
    shutdown_grace_seconds = 15

    [hermes.config.ingress]
    reorder_window_milliseconds = 120
    durable_accept_timeout_milliseconds = 100

    [hermes.config.routing]
    social_mode = "hybrid"
    sample_rate = 1.0
    decision_timeout_milliseconds = 2500
    decision_context_messages = 10
    decision_base_url = "https://open.bigmodel.cn/api/paas/v4"
    decision_model = "glm-4.5-air"
    ordinary_freshness_seconds = 8
    coalesce_window_milliseconds = 900
    ambient_cooldown_seconds = 20
    ambient_window_seconds = 60
    ambient_max_replies = 2

    [hermes.config.scheduler]
    router_workers = 2
    interactive_workers = 4
    interactive_reserved_workers = 1
    job_workers = 2
    tool_workers = 8
    max_active_sessions = 512

    [hermes.config.agent]
    python = "/root/.local/share/uv/tools/hermes-agent/bin/python"
    working_directory = "/root/hermes"
    startup_timeout_seconds = 60
    interactive_timeout_seconds = 30
    job_timeout_seconds = 600
    cancel_grace_seconds = 3

    [hermes.config.output]
    delivery_semantics = "at_least_once"
    send_timeout_seconds = 15
    retry_min_seconds = 1
    retry_max_seconds = 300
    send_interval_milliseconds = 700
    send_jitter_milliseconds = 250

配置校验失败时拒绝启动，不偷偷替换关键语义。热更新分为：

- 可立即生效：路由模式、采样率、并发和限流。
- 只影响新 Run：模型、提示词、工具配置。
- 需要 Drain 后重启：Python、协议版本、数据目录。

## 16. Go 扩展架构

扩展能力最后实现，但以下端口从第一阶段固定：

    InputAdapter
    OutputAdapter
    TurnRouter
    ControlRouter
    AgentEngine
    Tool
    MemoryStore
    MemoryIndex
    EventStore
    Renderer
    PolicyEvaluator
    Observer

模块接口：

    type Module interface {
        Manifest() Manifest
        Register(registry *Registry) error
    }

Manifest 包含：

- module_id。
- semantic_version。
- core_api_range。
- 提供的 Port。
- 依赖的 Port。
- 配置 Schema。
- 所需 Capability。

首期模块以 Go 编译期注册：

    registry.RegisterTool(...)
    registry.RegisterAgentEngine(...)
    registry.RegisterRouter(...)

不使用 Go 标准库 plugin 包，原因是它要求严格一致的 Go 版本、依赖图和构建参数，不适合稳定插件平台。

Registry 在启动时完成依赖图检查、循环检测和版本校验。运行期只读取不可变 Registry Snapshot。

## 17. 安全模型

- Run 使用不可变 Principal，不从“最新路由”反查权限。
- Owner 权限在 Ingress 验证后写入 RunContext，Agent 无法自行声明。
- Agent 只获得不透明 Session ID 和工具授权，不获得真实 wxid。
- 所有副作用在 Go 中再次校验 Principal、Session 和 Revision。
- Provider Secret 不进入 Prompt、事件日志或普通结构化日志。
- 日志默认不记录完整消息正文和工具 Secret。
- 网络工具默认禁止访问 Loopback、链路本地和私网地址，允许列表显式放行。
- 文件工具只允许配置根目录下的规范化路径。
- 媒体类型同时校验声明类型、Magic Bytes 和大小。
- Tool Result 有最大尺寸，防止 Worker 或上下文被大响应拖垮。

## 18. 生命周期与恢复

### 18.1 启动

1. 打开数据库并执行 Migration。
2. 校验配置和数据目录。
3. 恢复过期 Lease。
4. 将普通 leased Outbox Item 重置为 retry_wait；leased 视频直接进入 dead_letter。
5. 启动 Store Writer 和 Recovery Scanner。
6. 启动 Output Dispatcher。
7. 启动 Router 与 Agent Worker Pool。
8. 开始消费 Inbox。

### 18.2 卸载

1. 将插件置为 draining。
2. OnEvent 仍可在短窗口内完成持久化，但不启动新 Run。
3. 取消 Interactive 和 Job Run。
4. 等待协作式取消。
5. 终止未退出 Worker。
6. 停止 Output Lease，不删除 Pending Item。
7. 等待 Store Writer 提交。
8. 关闭数据库。

### 18.3 崩溃恢复

- accepted、ordered、routed Turn 继续调度。
- running Run 的 Lease 过期后从 Checkpoint 恢复或重跑。
- cancel_requested Run 恢复为 cancelled，不重新执行。
- 普通 leased Outbox Item 重新发送，符合至少一次语义；视频 Outbox 不重新发送。
- 未提交的 EffectProposal 不发送。
- 已提交 Job Revision 之外的迟到事件被拒绝。

## 19. 背压和过载

内存队列只保存数据库 ID，不保存完整消息。

过载策略：

- Control 永不被普通任务挤占。
- 明确私聊、@、引用使用保留 Interactive 容量。
- Social Router 队列饱和时普通群消息降级为 observe，但仍进入历史。
- Job 队列饱和时持久化排队并发送自然确认，不创建更多 goroutine。
- Tool 并发按工具名和会话双重限制。
- Output 持续失败时打开 Circuit Breaker，保留 Outbox，不拖垮 Agent。
- 数据库达到配置水位时停止接受可选后台工作，并通过状态命令告警。

首期建议容量：

- 活跃 Session Actor：512。
- Router 并发：2。
- Interactive Agent 并发：4，其中 1 个保留给明确交互。
- Job Agent 并发：2。
- Tool 并发：8。
- 普通群聊 freshness：5 秒。

这些值必须通过真实 Coding Plan 限流和服务器资源压测后调整。

## 20. 可观测性

每条链路统一关联：

    event_id
    session_id
    turn_id
    run_id
    job_id
    revision
    tool_invocation_id
    outbox_id

指标至少包括：

- Inbox 接受延迟和积压。
- 重排等待时间。
- 各 Lane 排队长度和等待时间。
- Router 决策延迟、超时、降级比例。
- Interactive 首回复延迟。
- Job 执行时间、取消延迟和修订次数。
- Worker 启动、崩溃、心跳失败和重启次数。
- Tool 延迟、错误和取消率。
- Outbox 积压、重试、重复风险和 Dead Letter。
- SQLite 写延迟和 Busy 次数。

提供 Owner 命令：

- /hermes status
- /hermes queues
- /hermes runs
- /hermes jobs
- /hermes cancel
- /hermes retry
- /hermes dead-letters
- /hermes health

状态命令只读取投影，不等待 Agent。

## 21. 测试策略

### 21.1 单元测试

- Inbox 去重和降级指纹。
- 重排窗口。
- Actor 状态机。
- Router 超时降级。
- Job Revision 和迟到结果拒绝。
- Outbox 重试和至少一次恢复。
- Config Snapshot 深拷贝。
- Capability 权限。

所有核心测试使用 Fake Clock、Fake Store、Fake Agent 和 Fake Output，不依赖真实微信或模型。

### 21.2 并发与性质测试

- 同一会话可见输出顺序不逆转。
- Job 永不阻塞 Interactive。
- Control 在有界时间内抢占。
- 取消后不再提交新的 EffectProposal。
- 旧 Revision 永远不能覆盖新 Revision。
- 队列达到上限时 goroutine 数保持稳定。
- Actor 回收后能从 Store 无损重建。

CI 必须开启 Go Race Detector；当前本机环境 CGO 关闭不能替代 CI 的竞态验证。

### 21.3 故障注入

在以下边界逐一 kill -9：

- Inbox 提交前后。
- Run Lease 前后。
- Tool Result 持久化前后。
- ReplyProposal 转 Outbox 前后。
- SDK Send 调用前、返回前和返回后。
- Outbox 标记 sent 前后。

验证：

- 已接受输入不消失。
- 未提交副作用不发送。
- 已提交 Outbox 最终会发送。
- 极端发送窗口只可能重复，不会静默丢失。

### 21.4 负载测试

模拟：

- 一个 100 人群，每秒 10 条普通消息，持续 30 分钟。
- 20 个私聊同时发送明确请求。
- 两个 10 分钟 Job 正在运行时持续闲聊。
- Router Provider 限流和超时。
- 一个 Python Worker 每分钟崩溃。
- 微信发送能力连续失败 5 分钟后恢复。

验收条件：

- 无无限 goroutine 增长。
- 无内存会话永久增长。
- Job 饱和不显著提高 Interactive 排队时间。
- 恢复后 Inbox 和 Outbox 自动收敛。
- 普通过时群聊不会延迟几十秒后突然回复。

## 22. 推荐目录结构

    plugins/hermes/
      main.go
      go.mod
      readme.md
      DESIGN.md
      internal/
        app/
          plugin.go
          lifecycle.go
        config/
          config.go
          snapshot.go
        domain/
          event.go
          turn.go
          run.go
          job.go
          outbox.go
        ingress/
          golem_adapter.go
          inbox.go
          reorder.go
        session/
          actor.go
          directory.go
          scheduler.go
        routing/
          router.go
          rules.go
          social_agent.go
          control.go
        runtime/
          engine.go
          protocol.go
          supervisor.go
          worker.go
        tools/
          registry.go
          broker.go
          history/
          memory/
          vision/
          search/
          render/
        output/
          outbox.go
          dispatcher.go
          golem_adapter.go
        store/
          store.go
          sqlite/
        observability/
          metrics.go
          logging.go
        modules/
          registry.go
      runtime/
        python/
          worker.py
          async_agent.py
          protocol.py
      migrations/
        001_initial.sql
      testkit/
        fake_agent.go
        fake_output.go
        fake_clock.go

禁止创建新的万能 helpers 或把领域逻辑重新堆回 Plugin 主结构体。

## 23. 实施阶段

### Phase 0：可验证内核

- 新插件骨架。
- Config Snapshot。
- SQLite Migration。
- Inbox、Turn、Run、Outbox 状态机。
- Fake Agent 与 Fake Output。

完成标准：不接真实模型也能通过崩溃恢复和顺序测试。

### Phase 1：快速文本闭环

- Golem Ingress。
- Session Actor。
- Rules、Hybrid、Agent 路由模式。
- Interactive Lane。
- 文本 Outbox。

完成标准：普通闲聊不受模拟长任务阻塞。

### Phase 2：异步 Python Agent

- 私有全双工协议。
- Worker Supervisor。
- 异步模型调用。
- 心跳、取消和超时。
- Interactive 与 Job 独立池。

完成标准：杀死单个 Job Worker 不影响其他会话。

### Phase 3：任务与工具

- Tool Broker。
- Job、Revision、Checkpoint。
- 补充、取消、进度。
- 历史、搜索和基础记忆。

完成标准：补充能在取消宽限期内停止旧 Revision，迟到结果不会发送。

### Phase 4：媒体和渲染

- 图片对象存储。
- 视觉分析。
- 图片、渲染 Outbox。
- 媒体安全策略。

### Phase 5：稳定性与压测

- 故障注入。
- Race、Goroutine Leak 和负载测试。
- Circuit Breaker 和 Dead Letter 管理。
- 可观测性命令。

### Phase 6：Go 扩展系统

- Module Manifest。
- Registry 依赖图。
- 配置 Schema。
- 示例 Tool、Router 和 Observer 模块。

扩展系统最后实现，但不得为了赶进度绕过前面已经定义的 Port。

## 24. 1.0 验收门槛

Hermes 1.0 只有同时满足以下条件才算完成：

1. 不修改 Host、SDK 和共享 Proto。
2. ai 插件可以继续存在，Hermes 使用独立配置和数据库。
3. OnEvent 不执行模型和慢工具。
4. 单个 Job 不阻塞同会话 Interactive。
5. Control 消息拥有最高优先级和真实取消传播。
6. Worker 一次只承载一个 Run，崩溃半径为一个 Run。
7. Agent 与 Go 之间使用版本化事件协议，不使用单次最终 JSONL。
8. Agent 不直接执行微信副作用。
9. 所有输出经过 Transactional Outbox。
10. 恢复采用至少一次发送，未完成 Outbox 不丢失。
11. 所有内存队列、Worker、Actor 和 goroutine 均有明确上限与生命周期。
12. 普通群聊 Agent 决策可配置为 observe、rules、hybrid 或 agent。
13. 100 人群聊负载场景通过，无持续内存和 goroutine 增长。
14. CI Race Detector、故障注入和恢复测试通过。
15. 旧 Revision、旧配置和迟到 Worker 事件都不能覆盖新状态。

## 25. 最终架构判断

Hermes 的核心不应是“一个更大的 AI 插件”，而应是一个小型、单机、事件驱动的对话内核：

- Golem 只是输入输出适配器。
- Python Hermes 只是可替换 Agent Engine。
- SQLite Event Store 是事实来源。
- Session Actor 决定顺序。
- Scheduler 决定资源。
- Agent 决定语义。
- Tool Broker 决定能力。
- Outbox 决定可靠发送。

只有把这些职责拆开，普通闲聊、长任务、取消、补充、工具和恢复才不会再次互相阻塞。
