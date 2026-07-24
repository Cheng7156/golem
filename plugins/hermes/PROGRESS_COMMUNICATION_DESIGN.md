# Hermes 长任务中途通信设计

状态：已在 `feat/hermes-progress-communication` 实现，待部署验收。

## 1. 目标

长工具链不能从用户发出请求后一直沉默到最终结果。对于私聊、@Hermes、引用 Hermes
等明确回合，系统需要支持三类自然语言消息：

1. 模型在第一个慢步骤前给出的简短确认；
2. 找到资源、开始下载、进入处理等真实阶段变化；
3. 长时间没有新阶段时的低频通用心跳。

中途消息不能结束 Run，不能绕过微信发送权限，也不能把工具名、参数、隐藏推理或内部错误
暴露给用户。ambient 群观察不得产生中途消息。

## 2. 源码结论

Hermes Agent 已经提供所需信号，不需要新增模型工具，也不需要修改 Hermes 核心源码：

- `agent.interim_assistant_callback` 会收到模型在工具调用前输出的自然 commentary；
- `display.long_running_notifications` 与 `agent.gateway_notify_interval` 提供低频心跳；
- Relay Observation V2 会把非最终 `send` 转为 `metadata.notify=false` 的进度帧；
- 最终结果仍通过 `commit_run_result_v1` 原子提交。

原问题位于 Golem Worker：`EventProgress` 原先和最终回复一起保存在内存 `drafts`，只有 Run
完成时才写入 Transactional Outbox。因此运行中看不到消息，失败时这些消息还会全部丢失。

不增加 `golem_progress_update` 一类模型工具。额外工具会增加一次模型工具循环，而且单个阻塞
工具执行期间模型仍然无法调用它；Hermes 已有的 commentary 与 heartbeat 更自然、更通用，
也没有额外推理开销。

## 3. 执行链路

```text
模型自然 commentary / Hermes generic heartbeat
  -> Relay send(metadata.notify=false)
  -> Golem EventProgress + pending proposal receipt
  -> Worker 校验 trigger、文本类型和条数
  -> SQLite run_progress_outputs + Outbox 同事务提交
  -> Worker 返回 CommandProposalResult
  -> Relay 仅在持久化成功后返回 outbound_result.success=true
  -> Output Dispatcher 立即发送微信

模型继续调用工具
  -> 最终 visible_reply / effect_only / observe
  -> 原有 durable Run result + final Outbox 事务
```

中途消息写入 Outbox 后立即唤醒 Dispatcher，不等待工具链结束。Relay 等待的是本地 SQLite
持久化确认，不等待微信网络发送，因此不会把微信延迟串行加入模型工具链。

## 4. 持久化与幂等

SQLite V12 新增：

```sql
run_progress_outputs(
  run_id,
  progress_id,
  content_hash,
  outbox_id,
  created_at
)
```

- `(run_id, progress_id)` 是主键；Relay request ID 经 SHA-256 生成稳定 progress ID。
- 相同 ID、相同内容返回原 Outbox 项，不重复插入。
- 相同 ID、不同内容返回 conflict。
- 同一 Run 的相同正文也映射到原 Outbox 项，且不新增别名行，既抑制网络超时或重复
  commentary 造成的刷屏，也保证 marker 数量受消息上限约束。
- 每个 Run 默认最多持久发送 8 条，可用 `output.progress_max_messages` 调整，上限 32。
- 只有文本可以作为进度；图片、视频、表情等仍只能走最终或已有异步投递链路。
- 进度提交不修改 Run、Turn 或 Inbox 的运行状态。

最终消息的 session sequence 排在已有进度之后。只要较早进度仍处于 `pending`、
`retry_wait`、`leased` 或 `sending`，Dispatcher 就不能租约后续最终消息；进度成功或进入
终态后才会继续，避免出现“先说完成，后说还在下载”。

## 5. 权限与交互规则

- 仅 `explicit` Run 允许中途消息；`ambient` 进度会被拒绝，但不会终止 Run。
- 进度使用当前 Run 已绑定的 session 与 receiver，Agent 不能指定新的微信目标。
- 群进度不重复 @ 发起者；最终回复仍保留原来的回复和 @ 目标。
- Relay platform hint 要求模型使用用户语言和当前人格语气，只在真实阶段变化时汇报。
- `tool_progress` 保持关闭，避免把原始工具名、命令和参数逐条发到微信。
- 普通工具错误返回模型，由 Agent 用自然语言总结；只有执行基础设施耗尽全部重试且模型无法
  生成最终回复时，Golem 才发送一条不包含 provider、路径、凭据或传输细节的通用失败消息。

## 6. 崩溃与失败语义

| 位置 | 结果 |
|---|---|
| SQLite 提交前崩溃 | Relay 未收到成功；该消息不会被谎报为已接受 |
| SQLite 提交后、ACK 前崩溃 | 重试由 progress ID/正文幂等映射到原 Outbox |
| ACK 后、微信发送前崩溃 | Outbox 重启后继续投递 |
| 微信暂时失败 | 使用现有 Outbox 退避重试，最终回复不能越过该进度 |
| 达到单 Run 条数上限 | 只拒绝新的进度，主 Run 继续执行 |
| ambient 产生进度 | 返回拒绝，不创建 Outbox，主 Run 继续执行 |
| Agent 能观察到的工具错误 | Agent 继续推理并自然报告 |
| 基础设施重试耗尽 | explicit Run 入队安全的通用失败回复；ambient 不发消息 |

文本投递沿用现有 at-least-once 语义。极端的“微信已收到但本地未记录 receipt”窗口仍可能
重复，这不是本次改动引入的新语义。

## 7. 性能

每条进度增加一次很短的 SQLite 写事务：一条 Outbox、一条 marker 和一次 session sequence
查询。SQLite 写路径已有进程内互斥，单 Run 又有 8 条默认上限，因此不会随工具调用数量无限
增长。模型不需要调用额外工具，也不等待微信 SDK，只等待本地持久化回执。

Outbox 顺序查询通过既有 `(session_id, sequence)` 唯一索引和 progress 的 `outbox_id` 索引
完成。历史 marker 对已结束消息不产生发送阻塞。

## 8. 推荐部署配置

Golem 插件：

```toml
[hermes.config.output]
progress_max_messages = 8
```

Hermes Agent：

```yaml
agent:
  gateway_notify_interval: 90

display:
  tool_progress: false
  interim_assistant_messages: true
  long_running_notifications: generic
  status_phrases:
    mode: replace
    phrases:
      status:
        - 我还在处理，结果出来就告诉你
        - 还在继续弄，暂时没有卡住
        - 这一步还需要一点时间，我处理完就回来
        - 还在等处理结果，有进展我马上说
        - 我还在跟进这件事，先别急
        - 任务还在继续，我没有消失
```

生产环境当前将 `gateway_notify_interval` 设为 `0`，并关闭了
`interim_assistant_messages` 和 `long_running_notifications`；部署本分支时必须同步调整，
否则持久进度链路虽已存在，Hermes 仍不会产生进度信号。

## 9. 验收标准

1. 发起一个至少跨越两个工具阶段的明确任务，第一条中途消息在 Run 仍为 `running` 时可见。
2. 最终结果只能在此前待发/重试中的进度之后出现。
3. 断开或重启插件后，已 ACK 的进度仍能从 Outbox 恢复。
4. 重放同一 progress ID 不产生第二条 Outbox；同正文重复 commentary 不刷屏。
5. 超过配置上限后主任务仍能完成并发送最终结果。
6. ambient 群消息不产生进度或失败兜底。
7. 日志中的 provider/adapter/path/token 等内部错误不会出现在微信回复中。
