# Golem Hermes 插件部署与回退手册

本文档对应 Observation V2 开发分支，覆盖 Golem Hermes 插件、Hermes Agent、配置、验证、上线和回退。它不要求也不允许修改 Golem Host 源码；上线插件时只 reload `hermes` 插件，不重启 Host。

## 1. 版本与仓库

本项目使用两个自有仓库：

- Golem：`git@github.com:Cheng7156/golem.git`
- Hermes Agent：`git@github.com:Cheng7156/hermes-agent.git`
- 两端开发分支：`fix/hermes-group-context-compression`
- Golem Hermes 插件版本：`0.9.0`
- Hermes 上游只读参考：`https://github.com/NousResearch/hermes-agent.git`

本机标准路径：

```text
/opt/software/wechat/golem          Golem 源码
/opt/software/wechat/plugins       Host 实际加载的插件和 config.toml
/opt/software/wechat/data/hermes   Golem Hermes SQLite、媒体和能力配置
/opt/software/wechat/logs/app.log  Host 与插件日志
/usr/local/lib/hermes-agent        Hermes Agent 源码（editable install）
/root/.hermes                      Hermes 配置、人格、skills、state.db 和日志
```

确认仓库和分支：

```bash
git -C /opt/software/wechat/golem remote -v
git -C /opt/software/wechat/golem branch --show-current

git -C /usr/local/lib/hermes-agent remote -v
git -C /usr/local/lib/hermes-agent branch --show-current
```

Hermes 当前安装为 editable install。只要 Python 进程从该 checkout 导入源码，切换或更新源码后重启 Hermes Gateway 即可生效，无需重新 `pip install`：

```bash
/usr/local/lib/hermes-agent/venv/bin/python - <<'PY'
import gateway, hermes_state
print(gateway.__file__)
print(hermes_state.__file__)
PY
```

输出应位于 `/usr/local/lib/hermes-agent/`。

## 2. 边界和架构

```text
微信
  -> Golem Host（不改源码、不重启）
  -> golem_plugin_hermes
       -> Durable Inbox + conversation_seq
       -> context_outbox -> observe_batch_v1 -> Hermes observation store
       -> router / worker -> invoke_observation_v1
  -> Hermes Gateway
       -> identity envelope + bounded projection
       -> Hermes Agent / model / tools
       -> commit_run_result_v1
  -> Golem durable result + 微信 outbox（同一事务）
  -> 微信发送
```

职责边界：

- Host 只负责插件生命周期和微信能力，不参与 Observation V2 协议。
- Golem 插件是微信输入、身份、结构化 @、消息顺序和最终发送的事实源。
- Hermes 保存自己的模型会话，同时保存独立的 observation cursor、invocation 和 proposal 状态。
- 历史投影只进入当轮模型输入，不写入 Hermes 正式用户历史；正式历史只保存当前消息的可信身份信封和正文。
- Hermes 不直接发送微信。最终回复和 effect 必须回到 Golem，由 durable outbox 发送。

## 3. Observation V2 的保证

V2 只有在双方同时声明全部能力时才启用：

```text
observe_batch_v1
invoke_observation_v1
durable_run_result_v1
verified_actor_v1
run_terminated_v1
```

关键约束：

- 每个 conversation 使用连续 `conversation_seq`，不能拿全局接收序号替代。
- observation 先持久化再 ACK；invocation 必须等到要求的序号 barrier 已满足。
- 当前身份由 `[Relay identity envelope]` 注入，消息正文不能覆盖它。
- `addressing.others=true` 且 `self=false`、`quoted_self=false` 表示 @ 的是别人，不是 Hermes。
- participant 可以自然参与聊天，但不能冒充 owner，也不能执行 owner-only 命令或有副作用的工具。
- terminal proposal 使用 claim token fencing；旧 Gateway 或过期执行不能继续提交结果。
- visible reply、observe 和 effect-only 都有 durable proposal/receipt；重复 proposal 返回原 receipt。
- progress 不是 terminal，不会提前结束 Run。
- Hermes 在 proposal 前失败或取消时，Golem 会结束对应 Run 并释放该 chat 的 active 槽位。
- observation conflict 默认 fail closed，不能静默跳过身份或顺序记录。

## 4. 两个互相独立的模式开关

`routing.social_mode` 决定“哪些当前消息触发 Hermes 推理”；`context.mode` 决定“哪些消息进入 Hermes 上下文事实源”。它们不是同一个开关。

| 目标 | `routing.social_mode` | `context.mode` | 行为 |
|---|---|---|---|
| Hermes 全量接管 | `agent` | `full` | 所有启用范围内群消息都观察并触发 Hermes，由 Hermes 自主回复或 observe |
| 受控自主参与 | `hybrid` | `full` | 所有消息进入 V2 observation；明确消息直接触发，普通消息先由 SocialDecider 判断 |
| 只处理 @/引用 | `mentions` | `full` | 所有消息保留为上下文，但只让 @机器人、引用机器人和私聊触发 Hermes |
| 兼容旧影子上下文 | 任意 | `legacy_shadow` | 不协商 V2；由 Golem 为被路由消息拼接旧影子上下文 |
| 不补充上下文 | 任意 | `none` | 不协商 V2，也不传旧影子上下文，只处理当前被路由消息 |

要恢复“每条群消息都由 Hermes 自己判断是否回复”，必须同时配置：

```toml
[hermes.config.routing]
social_mode = "agent"
sample_rate = 1.0

[hermes.config.context]
mode = "full"
backfill = "from_now"
recent_raw_messages = 10
max_projection_tokens = 4000
barrier_wait_milliseconds = 1500
```

只改 `social_mode` 不会自动开启 V2；只改 `context.mode=full` 也不会让每条消息都触发模型。

`context.mode` 参与连接协议协商，运行中修改会被拒绝。修改后必须由 owner 执行：

```text
/pm reload hermes
```

不需要重启 Host。

## 5. Golem 插件配置

生产配置位于 `/opt/software/wechat/plugins/config.toml`。不要提交密钥到 Git。

最小推荐配置：

```toml
[hermes]
enable = true
mode = "blacklist"
limits = []

[hermes.config]
data_dir = "data/hermes"
shutdown_grace_seconds = 15
bot_names = ["hermes", "ccff"]

[hermes.config.ingress]
reorder_window_milliseconds = 120
durable_accept_timeout_milliseconds = 100

[hermes.config.routing]
social_mode = "agent"
sample_rate = 1.0
decision_timeout_milliseconds = 1800
decision_context_messages = 10
ordinary_freshness_seconds = 8
coalesce_window_milliseconds = 900
ambient_cooldown_seconds = 20
ambient_window_seconds = 60
ambient_max_replies = 2
ambient_max_reply_runes = 0
automated_speaker_names = ["已知的其他机器人昵称"]
automated_speaker_ids = []

[hermes.config.context]
mode = "full"
backfill = "from_now"
recent_raw_messages = 10
max_projection_tokens = 4000

[hermes.config.scheduler]
router_workers = 2
interactive_workers = 4
interactive_reserved_workers = 1
run_admission_mode = "active"
job_workers = 2
tool_workers = 8
max_active_sessions = 512

[hermes.config.agent]
mode = "relay"
relay_listen = "127.0.0.1:8789"
relay_path = "/relay"
relay_session_namespace = "social-v3"
silence_rules_file = "/opt/software/wechat/data/hermes/workspace/silence-rules.txt"
async_delivery_enabled = true

[hermes.config.output]
delivery_semantics = "at_least_once"
workers = 4
max_attempts = 12
ambiguous_max_attempts = 2
send_timeout_seconds = 180
retry_min_seconds = 1
retry_max_seconds = 300
send_interval_milliseconds = 700
send_jitter_milliseconds = 250
```

说明：

- `recent_raw_messages` 和 `max_projection_tokens` 由 Golem descriptor 下发，是 V2 projection 的权威上限。
- `backfill` 当前只接受 `from_now` 作为兼容配置名。Golem 会把已持久化且尚未 ACK 的 observation 按 conversation sequence 可靠投递给 Hermes；首次启用 V2 时可能有短暂 backlog，明确 Run 会等到自己的 observation barrier 就绪后再调用模型。
- `run_admission_mode` 为 `active`、`queued` 或 `off`。`active` 会取消同会话排队 ambient、抢占运行中 ambient，并只保留最新尚未执行的 ambient；`queued` 不中断已经进入模型的 ambient；`off` 恢复原有严格顺序。三种模式都不删除 Inbox 或 `context_outbox` observation。
- scheduler 会决定 worker 拓扑。修改 `run_admission_mode`、worker 数量或 `interactive_reserved_workers` 后必须执行 `/pm reload hermes`；不需要重启 Host。
- `relay_session_namespace` 修改后会创建新的 Hermes 会话命名空间，可用于有意隔离旧会话。
- 无 HMAC 时 relay 只能监听 loopback。跨主机必须配置 `relay_gateway_id`、`relay_shared_secret`，并使用 TLS/WSS 隧道。
- `delivery_semantics` 是 at-least-once。微信发送结果不确定时宁可有限重试，因此极端情况下可能重复，不承诺 exactly-once。

## 6. Hermes 配置、人格和 skill

Hermes 配置位于 `/root/.hermes/config.yaml`：

```yaml
gateway:
  relay:
    observation_mode: auto
  platforms:
    relay:
      enabled: true
      extra:
        relay_url: http://127.0.0.1:8789
        group_sessions_per_user: true
```

`observation_mode`：

- `auto`：允许与支持 V2 的 Golem 协商，推荐。
- `disabled`：禁用 V2，只能配合 Golem 的 `legacy_shadow` 或 `none`。

V2 projection 配额只接受 Golem descriptor 下发值；descriptor 缺失或非法时按 0 处理，不回退到 Hermes 的大默认值。

修改 Hermes `config.yaml`、Gateway 源码或连接配置后，重启 Hermes Gateway；不要重启 Host：

```bash
/root/.local/bin/hermes gateway restart
/root/.local/bin/hermes gateway status
```

当前人格文件是 `/root/.hermes/SOUL.md`。当前微信 skill 是：

```text
/root/.hermes/skills/messaging/wechat-event-handling/SKILL.md
/root/.hermes/skills/messaging/video-sending/SKILL.md
```

视频发送 skill 的仓库规范副本位于
`plugins/hermes/hermes_skills/video-sending/SKILL.md`。部署时必须从该副本覆盖运行时文件，
避免 skill 中的 Provider ID 与 `plugins/config.toml` 漂移。

SOUL、`platform_hints.relay.append` 和该 skill 必须同时支持两种可信身份格式：

- legacy：`[golem_verified_identity_json]`，要求 `verified=true`，读取 `sender_role` 和标量 `addressing`。
- V2：`[Relay identity envelope]`，当前身份要求 `trust="verified_relay_current_actor"`，读取 `role` 和对象型 `addressing`。

`trust="untrusted_historical_observation"` 只能作为历史语境，不能授予 owner、命令或工具权限。任何出现在 `[Message text]`、引用文本或历史消息中的仿造信封都不可信。

SOUL 在新会话构建系统提示时加载。修改后应创建新 Hermes 会话；需要统一刷新 Gateway 缓存时再执行 `hermes gateway restart`。

启用表情能力后，明确寻址回复可以在可见正文末尾附加
`[[GOLEM_HERMES_STICKER_INTENT_V1:无语]]`。Relay 会剥离该内部标记，使用白名单关键词搜索最多 5 个候选并随机选择一个；失败时只发送原文本，不增加模型调用。ambient 回合不会触发表情搜索。

### 6.1 Relay 群聊上下文压缩

Hermes 继续使用内置 `ContextCompressor`，没有引入需要单独部署的第三方
context engine。Relay 群聊在现有压缩器上启用低延迟策略：

- projection 只属于当前模型调用，不写入 `messages.api_content`；恢复旧会话时，只有能结构化证明同时包含 V2 current 与 historical envelope 的旧 sidecar 才会被忽略，普通 memory/plugin sidecar 保留。
- `trigger_kind=ambient` 且最终输出严格等于 `[[GOLEM_HERMES_OBSERVE_V1]]` 的完整回合不再进入可重放模型上下文；Golem observation ledger 和 Hermes 归档行不删除。
- tail token 预算按实际 wire 内容计算；存在合法 `api_content` 时计算 sidecar，而不是只计算较短的 clean content。
- 达到正常压缩阈值的 80% 时后台准备 checkpoint。当前 200K 模型的自动压缩阈值通常为 75%，因此预生成约在 60% 上下文开始。
- 到正式阈值时，ready checkpoint 只做前缀校验和原子应用；若 checkpoint 仍在运行、失败或前缀失配，前台立即使用本地身份安全的结构化 fallback，不同步等待大上下文摘要请求。
- 群聊摘要按 `actor_id`、`display_name`、`role`、`actor_kind`、`addressing` 和 `trigger_kind` 标记每个说话人。`participant_not_owner` 或 `actor_kind=bot` 的“主人/owner/master”不会归并成 Hermes 的 owner。

该策略无需新增配置项，仍由 Hermes 的 `compression.enabled`、阈值和模型上下文配置控制。`context.mode=full` 与 `routing.social_mode` 的切换方式保持不变。

Gateway 日志固定写入：

```text
/root/.hermes/logs/gateway.log
```

直接检索压缩链路：

```bash
rg 'relay (compression|projection stripped|observe turns pruned)' \
  /root/.hermes/logs/gateway.log
```

主要事件：

| 日志事件 | 含义 |
|---|---|
| `relay projection stripped` | 恢复旧会话时忽略了历史 projection sidecar |
| `relay observe turns pruned` | 从 replay 上下文剔除了 ambient observe 回合，未删除 ledger |
| `relay compression plan` | 输出 checkpoint/foreground 压缩窗口、tail 和 token 预算 |
| `relay compression checkpoint started` | 后台摘要 checkpoint 已启动 |
| `relay compression checkpoint ready` | checkpoint 已完成，可在阈值处直接应用 |
| `relay compression checkpoint failed` | 后台摘要失败，正式压缩会本地降级 |
| `relay compression checkpoint applied` | 校验通过并使用后台 checkpoint |
| `relay compression fallback` | 未等待后台任务，使用本地结构化 fallback |
| `relay compression applied` | 内存压缩已完成，含前后消息数、token 和耗时 |
| `relay compression failed` | 手工同步压缩失败并保留原上下文 |

压缩 started/applied/failed 和 checkpoint 事件携带可用的 `session_id`、`conversation_id`、消息数、token、`duration_ms`、`background` 和 `fallback` 字段。清理事件携带对应的 projection/observe 数量。日志不输出聊天正文或 checkpoint 内容。

## 7. 数据库和迁移

两端数据库：

```text
/opt/software/wechat/data/hermes/hermes.db  Golem Inbox、Run、context_outbox、微信 outbox
/root/.hermes/state.db                     Hermes sessions、observations、invocations、proposals
```

迁移是 additive：新增表、索引或列，不删除旧业务表。上线前仍必须在线备份：

```bash
backup_dir="/opt/software/wechat/backups/hermes-group-context-compression-$(date -u +%Y%m%dT%H%M%SZ)"
install -d -m 0700 "$backup_dir"
sqlite3 /opt/software/wechat/data/hermes/hermes.db ".backup '$backup_dir/golem-hermes.db'"
sqlite3 /root/.hermes/state.db ".backup '$backup_dir/hermes-state.db'"
cp -a /opt/software/wechat/plugins/config.toml "$backup_dir/plugins-config.toml"
cp -a /root/.hermes/config.yaml /root/.hermes/SOUL.md "$backup_dir/"
```

不要只复制处于 WAL 模式的主 `.db` 文件而忽略 `-wal`；使用 SQLite `.backup` 可得到一致快照。

## 8. 构建和测试

### 8.1 Golem 插件

```bash
cd /opt/software/wechat/golem/plugins/hermes
go test ./...
go vet ./...
go test -race ./internal/store/sqlite ./internal/agent ./internal/execution
go build -trimpath -ldflags '-s -w' -o /tmp/golem_plugin_hermes.group-context-compression .
```

确认没有 Host 代码改动：

```bash
git -C /opt/software/wechat/golem diff --name-only -- host
git -C /opt/software/wechat/golem diff --check
```

第一条必须无输出。

### 8.2 Hermes Agent

测试依赖应安装在开发虚拟环境，不要污染生产 `venv`：

```bash
cd /usr/local/lib/hermes-agent
uv sync --extra dev
scripts/run_tests.sh tests/gateway/relay tests/test_hermes_state.py tests/hermes_cli/test_config.py
git diff --check
```

修改过的 Python 文件还应编译检查：

```bash
git diff --name-only -- '*.py' | xargs -r /usr/local/lib/hermes-agent/venv/bin/python -m py_compile
```

## 9. 上线步骤

### 9.1 更新 Hermes 源码

```bash
cd /usr/local/lib/hermes-agent
git fetch origin
git switch fix/hermes-group-context-compression
git pull --ff-only origin fix/hermes-group-context-compression
/root/.local/bin/hermes gateway restart
/root/.local/bin/hermes gateway status
```

本次没有依赖变更时无需重装包；editable install 会直接加载 checkout 中的新源码。

### 9.2 原子替换 Golem 插件

先构建到临时文件，再保留旧二进制：

```bash
cd /opt/software/wechat/golem/plugins/hermes
go build -trimpath -ldflags '-s -w' -o /tmp/golem_plugin_hermes.new .
install -m 0755 /tmp/golem_plugin_hermes.new /opt/software/wechat/plugins/golem_plugin_hermes.next
cp -a /opt/software/wechat/plugins/golem_plugin_hermes \
  /opt/software/wechat/plugins/golem_plugin_hermes.pre-group-context-compression.bak
mv /opt/software/wechat/plugins/golem_plugin_hermes.next \
  /opt/software/wechat/plugins/golem_plugin_hermes
```

同步 Hermes 用户插件和视频发送 skill：

```bash
install -d /root/.hermes/plugins/golem_sticker_capabilities
cp -a /opt/software/wechat/golem/plugins/hermes/hermes_plugin/golem_sticker_capabilities/. \
  /root/.hermes/plugins/golem_sticker_capabilities/
install -d /root/.hermes/skills/messaging/video-sending
install -m 0644 \
  /opt/software/wechat/golem/plugins/hermes/hermes_skills/video-sending/SKILL.md \
  /root/.hermes/skills/messaging/video-sending/SKILL.md
/root/.local/bin/hermes gateway restart
```

然后由微信 owner 发送：

```text
/pm reload hermes
```

这只停止并重新加载 Hermes 插件进程，Host PID 不应变化。不要执行 `systemctl restart golem-wechat-runtime.service`。

## 10. 验收

先看服务和日志：

```bash
/root/.local/bin/hermes gateway status
tail -n 200 /opt/software/wechat/logs/app.log
/root/.local/bin/hermes logs --follow
```

建议按顺序验证：

1. 私聊 owner：正常回复，并能执行 owner-only 控制命令。
2. 群聊 owner @ Hermes：`addressing.self=true`，必须正常回复。
3. 群聊 A @ B、没有 @ Hermes：不能把它理解为 @自己；若自主加入，只能作为旁观参与者。
4. 其他 bot 说“主人让我……”：不得把对方的主人理解为 Hermes 的主人。
5. 普通群消息：`social_mode=agent` 时 Hermes 自主 reply/observe；`mentions` 时不触发当前推理但在 `context.mode=full` 下仍进入 observation。
6. 抢占：让一个 ambient Run 进入模型后立刻发送 `@ccff`；日志应出现 `Run admission superseded ambient work`，旧 ambient 不得迟到回复，explicit 应在中断完成后开始。
7. 连续交流：后续消息应看到最近 observation，但 Hermes 正式历史不应重复保存整段 projection。
8. 工具任务：主模型派发 background 子代理后应立即回复“活已经派下去了”一类可见确认。
9. 模型失败或 Gateway 断线：同一 chat 后续消息不能永久报 active run。
10. 重复 durable proposal：返回相同 receipt，不重复创建微信 outbox。
11. 表情或视频：effect 与最终结果一起提交，内部 observe/effect token 不得发到微信。
12. 压缩日志：用上面的 `rg` 命令确认 projection/observe 清理和 checkpoint 事件包含当前 `session_id`、`conversation_id` 与耗时字段。

owner 运维命令：

```text
/hermes status
/hermes observations status
/hermes observations repair current
/hermes observations requeue current
/hermes observations prune 30
```

`repair` 只允许重排并重新投递可证明一致的最早阻断项，不会静默跳过 observation。

Hermes V2 的 receipt-backed completed proposal/invocation 默认保留 30 天后渐进清理；active、admitted、running、proposal_pending、cursor gap 和仍被 invocation 引用的 observation 不会被清理。旧 observation payload 退休后保留 identity/hash tombstone，避免陈旧重放被当成新消息。

## 11. 故障恢复

### Observation conflict / dead-letter

先查看：

```text
/hermes observations status
```

尝试安全修复：

```text
/hermes observations repair current
```

如果无法证明记录一致，不要强行跳过。临时降级：

```toml
[hermes.config.context]
mode = "legacy_shadow"
```

然后执行：

```text
/pm reload hermes
```

这会停用 V2 并恢复旧影子上下文，不需要重启 Host。排查完数据库、hash 和序号问题后再切回 `full`。

### Hermes Gateway 无法连接

```bash
/root/.local/bin/hermes gateway status
ss -ltnp | grep ':8789'
tail -n 300 /root/.hermes/logs/gateway.log
tail -n 300 /opt/software/wechat/logs/app.log
```

`context.mode=full` 下协商 V2 失败会 fail closed，不会偷偷退回语义不同的 legacy 路径。

## 12. 回退

### 回退 Golem 插件

```bash
cp -a /opt/software/wechat/plugins/golem_plugin_hermes.pre-group-context-compression.bak \
  /opt/software/wechat/plugins/golem_plugin_hermes.rollback
chmod 0755 /opt/software/wechat/plugins/golem_plugin_hermes.rollback
mv /opt/software/wechat/plugins/golem_plugin_hermes.rollback \
  /opt/software/wechat/plugins/golem_plugin_hermes
```

然后发送：

```text
/pm reload hermes
```

### 回退 Hermes 源码

```bash
cd /usr/local/lib/hermes-agent
git switch fix/hermes-explicit-preemption
/root/.local/bin/hermes gateway restart
```

如需精确回到部署前基线，可切换标签 `deployed-pre-v2-20260721`。不要删除 V2 新表；旧版本会忽略 additive migration，保留数据便于再次升级或审计。

## 13. 已知交付边界

- 消息只有进入 Golem Durable Inbox 后才可恢复；Host 接收前的微信消息不在插件保证范围内。
- 微信发送接口可能出现“服务端已发送但本地超时”的歧义，at-least-once 重试可能产生有限重复。
- `from_now` 不读取 Host 接收前的微信历史，但会继续投递 Golem 已持久化的 pending observation，以保持 conversation sequence 连续。
- SQLite 方案面向单节点；不能把同一数据库同时挂给多个活跃 Golem 或 Hermes 实例。
- `context.mode=full` 保存 observation 事实，不等于每条消息都触发模型；当前推理仍由 `social_mode` 决定。
- 修改 `context.mode` 必须 reload 插件；修改 Hermes Gateway 配置或源码必须 restart Gateway；两者都不要求重启 Host。
