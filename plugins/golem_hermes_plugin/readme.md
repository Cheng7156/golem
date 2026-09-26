# golem_hermes_plugin

把 Golem Host（微信）接入 [Hermes Agent](https://github.com/NousResearch/hermes-agent)——一个本地源码运行的通用 AI Agent。接入后你的微信群里会有一个：自然聊天的群友、可查可改配置的助手、会写日报的史官、认识每个成员的熟人，外加一个 Web 监控台。

- 版本：v0.7.0（配合 golem sdk v0.1.1、hermes-agent v0.21.1 开发）
- 许可：与 golem 主仓库一致
- 形态：单插件（独立 Go module），不修改 Golem Host / SDK / 共享 proto
- 外部依赖：[hermes-agent](https://github.com/NousResearch/hermes-agent)（本地源码或安装版，MIT）

---

## 一、它能做什么

| 能力 | 说明 |
|---|---|
| 自然群聊 | @/引用/免@白名单必答；环境闲聊按概率+群冷却随机参与；上下文环形缓冲注入（旁听但只回被点的） |
| 双通道分流 | `fast`（轻量快模型，秒回闲聊）/ `full`（重模型+全工具，处理任务）；关键词路由 |
| 微信工具桥 | agent 可调用：发表情、发图片（markdown 渲染）、引用回复、查群成员、查聊天记录、查最近上下文 |
| 会话级配置 | 每个群独立的静音/免@/概率/冷却/会话模式/史官开关——全部在 Web 页面行内修改，免改配置文件 |
| 群成员画像 | 消息归档 → 后台 LLM 抽取六类印象记忆 → 注入提示词自然带出；`remember` 工具对话中即时记忆；透明原则（问必答） |
| 情绪表情包库 | 自动收集群表情 → 视觉模型打情绪标签 → 按情绪概率点播/附带发送 |
| 群史官 | 每日定时出刊（太史公/说书人/新闻联播/武侠四文风轮换，Web 文风坊可编辑），六栏目报纸渲染成图；号外机制；期数存档可回看 |
| 傻吊三件套 | 🐓打鸣（每日随机清晨喔喔喔）、📓记仇小本本（被嫌弃入账、低频翻旧账）、🌫️突然的自我（深夜低概率emo后自动撤回） |
| 触发优先级 | 正式交互（@/引用/私聊）> 环境闲聊；闲聊永不抢占正式回复；同会话最新消息优先 |
| Web 监控台 | 十视图实时监控（概览/运行/决策/会话/用量/表情/画像/史官/日志/配置），决策流水可回答"这条消息为什么没回" |
| 治理 | 触发/运行全链路埋点、每日 token 用量记账与预算熔断、主人识别（协议级 wxid）与成员屏蔽、隐私抽取排除 |

## 二、架构

```
微信 ⇄ Golem Host (golem_lib.exe)
 └─ golem_hermes_plugin.exe（本插件，hashicorp/go-plugin 子进程）
     ├─ ingress   订阅 message::* → @/引用识别 → 去重 → 决策（毫秒级入队）
     ├─ context   每会话 ring buffer（旁听上下文注入）
     ├─ dispatch  同会话串行、跨会话并行、latest-wins 打断、触发优先级
     ├─ engine    HTTP/SSE → Hermes API Server（OpenAI 兼容端点）
     ├─ bridge    127.0.0.1:8790 工具桥 + Web 监控台（/ui）
     ├─ profile   消息归档 → 画像抽取 → prompt 注入
     ├─ chronicle 史官定时出刊
     └─ store     单 SQLite（表情库/归档/画像/用量/群设置/史官存档）
          ⇅ HTTP/SSE（127.0.0.1:8642）
     Hermes Agent gateway（本地进程，模型 provider 可插拔，本插件不感知具体模型）
```

关键设计约束（详见设计文档章节）：

1. **不修改 Host/SDK**：插件只依赖 `sdk` 公开接口；对 Hermes 的全部交互走其官方 HTTP API。
2. **OnEvent 同步约束**：宿主并发分发、插件 OnEvent 有超时——因此事件入口只做去重/决策/入队，重活全在后台 worker。
3. **会话隔离**：工具操作绑定"活跃会话"（显式头 > 唯一在途 run > 拒绝），agent 无法指定任意 wxid；多群并发时无绑定的调用会被 409 拒绝。
4. **隐私**：画像抽取提示词层强制丢弃手机号/住址/证件/转账类内容；出站文本兜底过滤 `MEDIA:`/本机绝对路径，服务器路径永不入群。

## 三、前置要求

| 组件 | 要求 |
|---|---|
| Go | 1.26+（构建插件） |
| Golem Host | 可正常登录微信、收发消息（本机实测 `golem_lib.exe` lib 模式） |
| [Hermes Agent](https://github.com/NousResearch/hermes-agent) | v0.21.1 实测可用；需 Python 3.11-3.13 + uv（或 pip） |
| LLM Provider | 任意 hermes 支持的 provider；实测智谱 `glm-4.7`（聊天）/`glm-5.3`（任务）/`glm-5.3-flash`（视觉）组合。注意 hermes 要求模型上下文 ≥64K |
| 端口 | 8642（hermes API server）、8790（本插件工具桥+监控台），均只绑 127.0.0.1 |

> Hermes 的安装与模型配置请参阅其官方文档（`hermes setup` / `hermes gateway`）。本插件只通过 HTTP 与其通信，**不需要**修改 hermes 源码。

## 四、快速开始

### 1. 构建

```bash
cd plugins/golem_hermes_plugin
go build -o golem_plugin_golem_hermes_plugin.exe .
# 或仓库根目录：task build:golem_hermes_plugin
```

把产物放到 **Golem Host 运行目录**下的 `plugins/`（宿主按工作目录解析，非仓库内 `host/plugins`）。

### 2. 部署 Hermes 侧（一次性）

```bash
# 2.1 hermes 主目录与 .env（Windows: %LOCALAPPDATA%\hermes）
HERMES_HOME=%LOCALAPPDATA%\hermes
cat >> "$HERMES_HOME/.env" <<'EOF'
GLM_API_KEY=<你的key>            # 或任意 hermes 支持 provider
GLM_BASE_URL=https://open.bigmodel.cn/api/coding/paas/v4
API_SERVER_ENABLED=true
API_SERVER_KEY=<自定16位以上密钥>   # golem 插件访问 hermes 用
GOLEM_BRIDGE_URL=http://127.0.0.1:8790
GOLEM_BRIDGE_TOKEN=<自定随机长串>   # hermes 回调 golem 工具桥用
EOF

# 2.2 部署 hermes 侧插件（把 golem 工具暴露为 agent 工具）
cp -r plugins/golem_hermes_plugin/hermes_side/golem_bridge "$HERMES_HOME/plugins/"
hermes plugins enable golem_bridge

# 2.3 让 golem 工具不被 tool_search 延迟隐藏（重要）
cat >> "$HERMES_HOME/config.yaml" <<'EOF'
tools:
  tool_search:
    defer: []
EOF

# 2.4 启动 hermes 网关（API server 随之启动）
hermes gateway
```

### 3. 配置插件

在宿主运行目录 `plugins/config.toml` 写入（首启也会自动生成默认值）：

```toml
[golem_hermes_plugin]
enable = true

[golem_hermes_plugin.config]
base_url = "http://127.0.0.1:8642"
api_key = "<与 .env 的 API_SERVER_KEY 一致>"
fast_profile = "fast"        # 可选：多 profile 分流（需在 hermes 侧创建同名 profile）
full_profile = "full"
vision_profile = "vision"    # 带图消息与表情打标走视觉模型
bridge_listen = "127.0.0.1:8790"
bridge_token = "<与 hermes 侧 GOLEM_BRIDGE_TOKEN 一致>"
```

### 4. 运行与验收

```bash
./golem_lib.exe      # 扫码登录，插件自动加载
```

- 群里 @ 它说句话 → 收到回复即链路通
- 打开 `http://127.0.0.1:8790/ui`（令牌=bridge_token）→「决策」页能看到这条消息的完整判定路径
- 完整验收清单见下文「验收清单」

## 五、配置参考

配置分两级：**config.toml 全局默认**（本文件，热更新）+ **群级覆盖**（Web 会话页 ⚙ 行内修改，存 SQLite）。决策时群级覆盖 > 全局默认；`nil` 语义即"跟随全局"。

### 全局（config.toml → `[golem_hermes_plugin.config]`）

| 键 | 默认 | 说明 |
|---|---|---|
| base_url / api_key | — | Hermes API server 地址与密钥 |
| fast/full/vision_profile | fast/full/vision | 通道 profile 名（带图与打标走 vision） |
| bridge_listen / bridge_token | 127.0.0.1:8790 / — | 工具桥+监控台监听与令牌 |
| ambient_reply_rate | 0.10 | 群环境闲聊参与概率 |
| ambient_cooldown_sec | 300 | 同群环境参与最小间隔 |
| min_ambient_len | 6 | 环境参与最小消息长度 |
| free_response_chats | [] | 免@白名单（群wxid） |
| full_keywords | （见源码） | 命中即走任务通道的关键词 |
| context_window / context_max_chars | 60 / 1500 | 每会话旁听上下文条数与注入上限 |
| run_timeout_sec / outbound_slots / max_parallel_sessions | 300 / 4 / 4 | 运行超时、出站并发、并行会话 |
| min_reply_interval_sec / max_reply_chars / max_segments | 3 / 1200 / 3 | 防刷屏三件 |
| daily_token_cap | 0 | 每日 token 上限（超限降级，0=不限） |
| owner_wxids / owner_only_private / owner_bypass_ambient | — | 主人识别追加/私聊门控/主人免@直答 |
| group_sessions_per_user | false | 群会话按成员拆分 |
| profile_enabled / profile_threshold / profile_batch / profile_inject | true/20/3/true | 群成员画像 |
| crow_enabled / grudge_enabled / mood_enabled | true/true/true | 傻吊三件套全局默认 |
| chronicle_enabled / chronicle_hour / chronicle_minute | true/22/0 | 史官总开关与出刊时刻 |
| chronicle_extra_threshold | 500 | 号外触发阈值（当日群消息量） |
| chronicle_style_mode / chronicle_fixed_style / chronicle_styles | rotate/太史公体/四预置 | 文风策略与文风池（Web 文风坊可编辑） |
| sticker_* | 见源码 | 表情收集/概率/冷却/情绪集合 |
| progress_hint_sec | 20 | 任务超时进度提示（0=关） |

### 群级（Web 会话页 ⚙，存 SQLite，即时生效）

`mute`（整群静音，仅主人@穿透）· `free_response` · `ambient_rate` · `ambient_cooldown_sec` · `chronicle` · `session_mode`（shared/per_user）· `crow_enabled` · `grudge_enabled` · `mood_enabled`

成员级（Web 画像详情）：`owner`（动态主人）· `ignored`（屏蔽，决策层全类型忽略）。

## 六、Web 监控台

`http://127.0.0.1:8790/ui`，登录令牌=bridge_token（HttpOnly Cookie，12h）。

| 视图 | 内容 |
|---|---|
| 概览 | 在途运行/活跃会话/今日token与预算水位/触发率 KPI，通道探活（15s），24h 各通道 P50/P95，今日决策环图，SSE 每秒刷新 |
| 运行 | 每次运行的触发/通道/耗时/token/状态；行内抽屉展示身份信封、回复全文、工具调用、分段与表情 |
| 决策 | 每条消息的判定路径：响应(@/引用/白名单/环境命中) 或 忽略(冷却/概率未中/预算熔断/去重/非触发类型/屏蔽/静音) |
| 会话 | 按聊天统计 + ⚙ 行内群配置（静音/免@/概率/冷却/会话模式/史官出刊）+ 上下文查看器 |
| 用量 | 每日 prompt/completion 堆叠趋势、按通道分解 |
| 表情 | 缩略图网格、情绪分布、打标队列、重打标 |
| 画像 | 成员卡片墙 + 角色卡抽屉（人设/印象时间线/活跃热力/表情倾向/设主人/屏蔽/重跑/删除） |
| 史官 | 编务台（出刊时刻/号外阈值/立即出刊/单群出刊/停刊）、旧报纸期卡墙、阅读模态、文风坊 |
| 日志 | 插件 slog 环形缓冲（500条）实时流 |
| 配置 | 生效配置全览（密钥打码、只读；写操作在各视图行内完成） |

## 七、安全模型

1. **监听仅本机**：bridge/监控台绑 127.0.0.1；开放局域网需自行评估（HTTP 明文 + 令牌单一因素）。
2. **两套鉴权并存**：hermes→插件走 `Authorization: Bearer bridge_token`（工具桥）；浏览器走登录换 HttpOnly Cookie（12h）。
3. **会话绑定防越权**：工具操作只能作用于"活跃窗口内的会话"（显式会话键 > hermes 会话ID反查 > 唯一在途run）；其余一律拒绝。agent 无法让消息发到任意 wxid。
4. **主人识别**：协议层 wxid 比对（宿主 Owner 自动并入 + `owner_wxids` 追加），与消息文本自述无关；发言昵称经控制字符消毒防信封伪造。
5. **隐私**：画像抽取排除敏感字段；出站兜底过滤本机路径；透明原则——任何群友问"你记了我什么"必须如实展示，监控台可删除任意画像。
6. **审计**：所有 Web 写操作（群设置/成员标记/重打标/画像删除/史官编务）记入插件日志。

## 八、目录结构

```
golem_hermes_plugin/
├─ main.go config.go incoming.go dispatch.go engine.go outflow.go bridge.go
│  context.go settings.go store.go metrics.go profile.go chronicle.go fun.go commands.go
├─ ui.html                    # 监控台单页（go:embed；hermes_data/ui.html 可覆盖热更新）
├─ ui_regression.js           # 前端全视图回归脚本（Console 粘贴执行，21+ 断言）
├─ *_test.go                  # 单测与 httptest 集成测试
└─ hermes_side/golem_bridge/  # hermes 侧用户插件（部署到 <HERMES_HOME>/plugins/）
   ├─ plugin.yaml
   └─ __init__.py             # register_tool ×7 + 系统提示注入（微信行为准则）
```

## 九、测试

```bash
go vet ./...
go test -count=1 ./...          # 单测 + httptest 集成（登录/鉴权/端点/数据正确性/令牌打码/画像/设置覆盖）
GOLEM_UI_HARNESS=1 go test -run TestUIHarness -v .   # 可选：8791 起带模拟数据的监控台供浏览器手测
```

前端改动请在浏览器执行 `ui_regression.js`（全视图断言）后再提交。

## 十、验收清单（真机）

- [ ] 私聊问答、群 @ 问答、群环境闲聊概率参与
- [ ] 引用 bot → 引用格式回复（gg 未部署时自动降级普通文本）
- [ ] @ 它要表情 → agent 调 send_emoji 发出库存表情
- [ ] `/ghp status`（状态）/ `/ghp whoami`（主人识别核对）/ `/ghp sticker count`
- [ ] 群表情自动收集 → 画像页/表情页出现，几秒内完成打标
- [ ] 会话页 ⚙ 开启史官 → 史官页「▶ 出刊」→ 群里收到报纸图
- [ ] 决策页能看到任意一条消息的"为什么回/为什么没回"

## 十一、已知限制

- 引用回复走 App XML（宿主 Send 分支不认 TypeAppQuote），微信客户端实际渲染以真机为准；失败自动降级普通文本
- `statistics.query_messages` 仅存文本；本插件自建归档表弥补（含昵称/类型），但重启前的实时窗口依赖 ring buffer
- 表情打标优先用表情名（Desc）文本分类，无名字表情走视觉模型；若所用 provider 无视觉能力则该表情停留在待打标（3 次后不再自动重试，可 `/ghp sticker retag`）
- 多群并发时工具桥要求精确绑定；无法绑定时 agent 会被告知拒绝（设计行为）
- 监控台/工具桥仅本机监听；跨机访问不在本项目安全边界内

## 十二、常见问题

- **决策页显示"忽略：群冷却中/概率未中"** —— 环境闲聊的正常节流，调 `ambient_reply_rate`/`ambient_cooldown_sec`（全局或该群 ⚙ 覆盖）
- **agent 说"表情包通道抽风"** —— 看运行抽屉的错误：令牌无效=两侧 token 不一致；会话拒绝=隔离闸门生效
- **回复里出现文件路径（v0.7.0 前）** —— 已修复：`MEDIA:`/本机路径会被兜底转为图片发送或静默丢弃
- **打鸣没响** —— 打鸣窗口 6:30-9:30，每日时刻由群ID+日期种子固定；先确认会话页 ⚙ `crow_enabled` 已开
