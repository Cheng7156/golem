# Hermes 后台子代理异步回流设计

状态：已实施

分支：`codex/hermes-async-delegation`

版本基线：Hermes Agent `0.18.2 / v2026.7.7.2`

## 1. 问题与边界

Hermes `delegate_task(background=true)` 返回后，父 Turn 会正常结束。子代理完成时，Hermes 再创建一个 internal synthetic Turn；此时 Golem 中的父 Run 已经终态，普通 Relay `send` 会被 `no active run for chat` 拒绝。

不能放宽普通 orphan send。Relay `requestId` 每次重试都会变化，也无法区分合法 completion、旧 Run 迟到重试和伪造发送。因此异步 completion 使用独立 capability HTTP 提交，普通 Relay 规则保持不变。

本阶段只支持：

- Hermes `0.18.2`。
- 文本最终结果。
- 运行中的 Hermes Gateway 进程；Hermes 自身不持久化 detached 子代理。
- Golem 插件 reload 后继续投递已经进入 Outbox 的结果。

## 2. 最终架构

```text
微信消息
  -> Golem Durable Inbox / Turn / parent Run
  -> Hermes Relay inbound
  -> delegate_task(background=true)
  -> Python tool_execution middleware
  -> POST /capabilities/v1/async-delivery/register
  -> Golem 校验 active parent Run 并持久化 ticket
  -> parent Run 正常提交

Hermes async_delegation completion event
  -> 版本锁定的 _inject_watch_notification 包装器
  -> 查询 ticket 状态；revoked/abandoned 不创建 synthetic Turn
  -> 真实 event 设置 ContextVar
  -> busy session 时进入独立 completion FIFO，不与普通 pending message 合并
  -> 普通 pending 链排空后，由 session guard 清理点原子接管下一条 completion
  -> Hermes synthetic Turn 整理最终文本
  -> completion 执行期间的新用户消息进入独立 deferred-user FIFO
  -> completion 提交并释放 guard 后，以空 ContextVar 启动用户 Turn
  -> 版本锁定的 GatewayRunner._handle_message 包装器取得完整最终文本
  -> synthetic Turn 内普通 RelayAdapter.send 全部显式拒绝
  -> POST /capabilities/v1/async-delivery/deliver
  -> 单事务写 synthetic Inbox + Turn + background Run + Outbox
  -> 现有 Dispatcher -> message.send -> 微信
```

异步 deliver 不走普通 Relay outbound，避免 WebSocket ACK 歧义和随机 `requestId`。HTTP 请求携带稳定 ticket，客户端只重试相同的幂等请求。

## 3. 票据和认证

Python 插件生成随机 `adt_...` ticket。Golem 仅存 SHA-256 摘要，并绑定：

- Relay 显式 profile；单 named profile 下 Relay 留空时使用 Hermes active profile；
- producer epoch、delegation id；
- Hermes session id、Relay session key、chat id；
- parent Run id；
- 从 `parent_run_id -> turns -> inbox_events` 反查出的 Golem session、Receiver 和完整 ChannelBinding。

登记必须发生在父 Run 仍为 `running` 时。相同 `(profile, producer_epoch, delegation_id)` 登记幂等返回原 ticket 状态；字段不一致则冲突。新 Gateway epoch 启动时逐个 reconcile 当前 Gateway 服务的全部 profile，把各 profile 的旧 pending ticket 标为 `abandoned`。

所有 capability 端点继续使用 `GOLEM_CAPABILITIES_TOKEN` Bearer 鉴权。异步能力和表情能力解耦：表情关闭时，只要异步开关开启，token 和端点仍会加载。

## 4. 数据与事务

`async_delivery_tickets` 保存 ticket 摘要、全部路由绑定、状态和结果 ID。状态机：

```text
pending -> consumed
pending -> revoked
pending -> abandoned
```

`CommitAsyncDelivery` 在一个 SQLite 写事务内：

1. 按 ticket 摘要读取并校验所有绑定。
2. `consumed` 返回原 message/outbox ID，不重复写入。
3. `revoked/abandoned` 返回 `discarded`，不写审计链或 Outbox。
4. 写 `inbox_events(done)`、`turns(completed, async_delivery)`、`runs(succeeded, background)`。
5. 命中现有群聊静默规则时 consume ticket，但不写 Outbox。
6. 其他文本先经过现有 Markdown-to-WeChat 清洗，再写 pending text Outbox。
7. 更新 ticket 为 consumed 后提交，并唤醒现有 Dispatcher。

最终微信发送仍由 Transactional Outbox 和 `message.send` 完成。

## 5. Hermes 兼容桥

Python 用户插件在启用异步能力时检查已安装包必须为 `hermes-agent==0.18.2`，并校验以下方法签名：

- `GatewayRunner._inject_watch_notification(self, synth_text, evt)`
- `GatewayRunner._build_process_event_source(self, evt)`
- `GatewayRunner._handle_message(self, event)`
- `GatewayRunner._is_user_authorized(self, source)`
- `GatewayRunner._adapter_for_source(self, source)`
- `GatewayRunner._handle_active_session_busy_message(self, event, session_key)`
- `BasePlatformAdapter.handle_message(self, event)`
- `BasePlatformAdapter._start_session_processing(self, event, session_key)`
- `BasePlatformAdapter._process_message_background(self, event, session_key)`
- `BasePlatformAdapter._cleanup_finished_session_task(self, session_key, interrupt_event)`
- `BasePlatformAdapter.cancel_background_tasks(self)`
- `RelayAdapter.send(self, chat_id, content, reply_to, metadata)`

不匹配时插件加载失败并暴露错误，不修改 site-packages。

兼容桥行为：

- `tool_execution` 只处理真实 `delegate_task` 返回的 `delegation_id`。
- 使用 middleware 的 `session_id`，不依赖 v0.18.2 未设置的 `HERMES_SESSION_ID`。
- completion 早于 HTTP 登记时等待进程内登记屏障。
- `registering` 状态绑定 Hermes session；reset 与 HTTP 登记并发时，迟到 ticket 保持 inactive 并立即撤销，不能重新变为 ready。
- 只有真实 `evt.type == async_delegation` 能建立 ContextVar；用户伪造文本无效。
- synthetic source 从 ticket 恢复原 profile，确保 named/multiplex profile 的配置、人格和 session namespace 不串线。
- ticket 非 pending 时不创建 synthetic Turn，防止旧结果污染 reset 后 transcript。
- busy session 的 completion 使用独立 FIFO；普通用户 pending turn 优先排空，二者不覆盖、不拼接。
- completion 执行期间到达的用户消息进入 deferred-user FIFO，不能被同一次 `_handle_message` 递归消费。
- completion 真正执行时从事件恢复 ContextVar；session boundary 后已失效的排队事件不启动模型。
- adapter shutdown 先阻止 queue handoff，再取消原生 background task；正在执行和排队的 completion 原始事件重新进入 Hermes process queue，不能静默丢失。
- synthetic Turn 禁止再次调用 `delegate_task` 和全部 Golem sticker 工具。
- synthetic Turn 的流式片段、工具进度和普通 Relay send 均返回明确失败，不能消费 ticket。
- 只有 `_handle_message` 返回的完整最终文本可以提交；Relay 的 2000 字流式拆块不会截断异步结果。
- `transform_llm_output` 去除 Markdown 图片、全部 Hermes 附件扩展名、`MEDIA:` 和附件标记，确保第一阶段只走文本。
- 网络错误、超时、HTTP 408/425/429 和 5xx 使用同一 ticket 重试；session boundary 会终止该重试循环。鉴权、绑定冲突、ticket 不存在和非法响应属于永久错误，会标记本地登记失败并释放 session，不会无限阻塞群消息。
- 仅明确处于 `registering` 的同进程竞态允许 completion 重排；无本地登记的旧进程/orphan completion 记录错误后丢弃，不能形成永久 requeue loop。

## 6. 会话边界

- `/new`、`/reset`：`pre_gateway_dispatch` 先调用 Hermes 原生授权检查，通过后立即使旧 session 的本地 ticket 失效；远端 revoke 依据 ticket 自身 profile，在 Gateway 事件循环外执行并显式记录失败。
- `/stop`：撤销当前 session ticket，并版本锁定地访问 `tools.async_delegation._records/_records_lock`，只调用相同 `session_key` 的 `interrupt_fn`；不使用全局 `interrupt_all()`。
- Gateway 新进程：新 producer epoch 启动 reconcile，旧 pending ticket 变为 abandoned。
- 登记失败：工具结果改为明确的 `delivery_registration_failed`，并中断对应 detached delegation，不伪装成功。

## 7. 配置

Golem 配置，默认关闭：

```toml
[hermes.config.agent]
async_delivery_enabled = true
```

Hermes `.env`：

```dotenv
GOLEM_ASYNC_DELIVERY_ENABLED=true
GOLEM_CAPABILITIES_URL=http://127.0.0.1:8789
GOLEM_CAPABILITIES_TOKEN=与Golem能力环境文件相同的令牌
GOLEM_CAPABILITIES_TIMEOUT_SECONDS=10
```

Hermes `config.yaml`：

```yaml
platform_toolsets:
  relay:
    - delegation
    - web
    - file
    - skills
    - golem_stickers
    - no_mcp
```

`web`、`delegation` 和 `golem_stickers` 可以共存。若表情能力关闭，可删除 `golem_stickers`，异步 delegation 不受影响。

## 8. 部署与验证

Go 插件更新只使用 `/pm unload/load/reload hermes`，不停止或重启 Golem Host。Python 用户插件更新后重启 Hermes Gateway。

自动化验证：

```powershell
cd plugins/hermes
go test -timeout 60s ./...
python hermes_plugin/golem_sticker_capabilities/test_plugin.py
python hermes_plugin/golem_sticker_capabilities/test_async_delivery.py
python hermes_plugin/golem_sticker_capabilities/test_async_delivery_queue.py
python hermes_plugin/golem_sticker_capabilities/test_async_delivery_security.py
python hermes_plugin/golem_sticker_capabilities/test_async_delivery_profiles.py
```

测试覆盖 active Run 登记、父 binding 反查、登记/提交幂等、commit/revoke 原子竞态、V1→V2 迁移、Outbox 重启恢复、错误绑定拒绝、静默/revoked/abandoned 无 Outbox、控制端点鉴权与参数校验、普通 Relay 不变、named/multiplex profile、真实 completion 身份、伪造文本隔离、busy session 双向隔离、shutdown 重排与失效丢弃、可重试与永久错误分类、未授权 reset/stop 拒绝、登记期间 reset、会话级中断、长文本单次提交、text-only 输出和版本签名失败。
