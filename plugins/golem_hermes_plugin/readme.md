# golem_hermes_plugin

把 Golem Host（微信）接入 [Hermes Agent](https://github.com/NousResearch/hermes-agent)——一个本地源码运行的通用 AI Agent。接入后你的微信群里会有一个：自然聊天的群友、可下订单的画师、会写日报的史官、认识每个成员的熟人、冷场时主动冒泡的活人，外加一个 Web 监控台。

- 版本：v0.10.1（配合 golem sdk v0.1.1、hermes-agent v0.21.1 开发）
- 许可：与 golem 主仓库一致
- 形态：单插件（独立 Go module），不修改 Golem Host / SDK / 共享 proto
- 外部依赖：[hermes-agent](https://github.com/NousResearch/hermes-agent)（本地源码或安装版，MIT）；画坊需自备生图服务商（OpenAI images 兼容 API）

---

## 一、它能做什么

| 能力 | 说明 |
|---|---|
| 自然群聊 | @/引用/免@白名单必答；环境闲聊按概率+群冷却随机参与；上下文环形缓冲注入（旁听但只回被点的） |
| 双通道分流 | `fast`（轻量快模型，秒回闲聊）/ `full`（重模型+全工具，处理任务）；关键词路由；带图走 `vision` |
| 微信工具桥 | agent 可调用：发表情、发图片（markdown 渲染）、引用回复、查群成员、查聊天记录、查最近上下文、即时记忆、查画像、**画坊下单**、**发画像卡** |
| 会话级配置 | 每个群独立的静音/免@/概率/冷却/会话模式/史官/三件套/冷场看护/画坊/人格——全部在 Web 页面行内修改，免改配置文件 |
| 群成员画像 | 消息归档 → 增量语料后台抽取印象记忆（变体去重）→ 注入提示词自然带出；**画像卡预渲染缓存**，群内毫秒级发送 |
| 情绪表情包库 | 自动收集群表情 → 视觉模型打情绪标签 → 按情绪概率点播/附带发送；**每聊天硬节流**（冷却期拒绝后 agent 不再重试） |
| 画坊 🎨 | 群里下生图/改图工单：桥接毫秒级受理 → 后台异步作画 → 交稿发群；防复读四道闸（库存直发 0 元/相似拒单/单人限购/全局产能）；@别人的单不接 |
| 群史官 | 每日定时出刊（太史公/说书人/新闻联播/武侠四文风轮换，Web 文风坊可编辑），报纸渲染成图；号外机制；**出刊去重落库**，reload 不重发 |
| 傻吊三件套 | 🐓打鸣（每日随机清晨喔喔喔）、📓记仇小本本（被嫌弃入账、低频翻旧账）、🌫️突然的自我（深夜低概率emo后自动撤回） |
| 冷场看护 🗣️ | 群沉默超过会话级阈值后主动冒泡：**说不说归代码**（双时钟/streak/日上限/静默时段/失败退避），**说什么归 agent**（回到沉默发生时的会话，带着它自己的记忆接话茬） |
| 人格面具 🎭 | 人设库（预置 5 种+Web 自建），每群可指定不同人格，情绪与语气随群而变 |
| 触发优先级 | 正式交互（@/引用/私聊）> 环境闲聊；闲聊永不抢占正式回复；同会话最新消息优先 |
| Web 监控台 | 十二视图实时监控（概览/运行/决策/会话/用量/表情/画像/史官/画廊/人设/日志/配置），决策流水可回答"这条消息为什么没回" |
| 治理 | 触发/运行全链路埋点、每日 token 记账与预算熔断、主人识别（协议级 wxid）与成员屏蔽、隐私抽取排除 |

## 二、架构

```
微信 ⇄ Golem Host (golem_lib.exe)
 └─ golem_hermes_plugin.exe（本插件，hashicorp/go-plugin 子进程）
     ├─ ingress   订阅 message::* → @/引用识别 → 去重 → 决策（毫秒级入队）
     │            └ imgmeta   从 Raw 报文抢救图片元数据（宿主解析零值时的兜底）
     ├─ context   每会话 ring buffer（旁听上下文注入）
     ├─ dispatch  同会话串行、跨会话并行、latest-wins 打断、触发优先级、人格注入
     ├─ engine    HTTP/SSE → Hermes API Server（OpenAI 兼容端点）
     ├─ bridge    127.0.0.1:8790 工具桥 + Web 监控台（/ui）
     ├─ imagegen  画坊：工单 SQLite + 后台画手 + 防复读闸
     ├─ fun       30s 心跳：打鸣/记仇/深夜emo/冷场看护
     ├─ profile   消息归档 → 增量画像抽取 → 画像卡预渲染
     ├─ chronicle 史官定时出刊（正刊/号外，DB 防重发）
     └─ store     单 SQLite（表情库/归档/画像/用量/群设置/史官存档/工单/人设）
          ⇅ HTTP/SSE（127.0.0.1:8642）
     Hermes Agent gateway（本地进程，模型 provider 可插拔，本插件不感知具体模型）
```

关键设计约束：

1. **不修改 Host/SDK**：插件只依赖 `sdk` 公开接口；对 Hermes 的全部交互走其官方 HTTP API。
2. **OnEvent 同步约束**：宿主并发分发、插件 OnEvent 有超时——事件入口只做去重/决策/入队，重活全在后台 worker。
3. **确定性代码兜底定律**：一切速率/消耗/去重/防刷屏的硬闸都在 Go 代码里（配置读写、配额、Jaccard 查重、节流、出刊去重），LLM 提示词只是软性第一道门——历史上三场"风暴"（号外重发、表情刷屏、画像注水）全部因此修平。
4. **会话隔离**：工具操作绑定"活跃会话"，agent 无法指定任意 wxid；多群并发时无绑定的调用被 409 拒绝。
5. **隐私**：画像抽取提示词层强制丢弃手机号/住址/证件/转账类内容；出站文本兜底过滤 `MEDIA:`/本机绝对路径；透明原则（问必答，监控台可删）。

## 三、前置要求

| 组件 | 要求 |
|---|---|
| Go | 1.26+（构建插件） |
| Golem Host | 可正常登录微信、收发消息（本机实测 `golem_lib.exe` lib 模式） |
| [Hermes Agent](https://github.com/NousResearch/hermes-agent) | v0.21.1 实测可用；需 Python 3.11-3.13 + uv（或 pip） |
| LLM Provider | 任意 hermes 支持的 provider；实测智谱 `glm-4.7`（聊天）/`glm-5.3`（任务）/`glm-5.3-flash`（视觉）组合。hermes 要求模型上下文 ≥64K |
| 生图服务商（可选） | OpenAI images 兼容 API（同步/异步任务制均可），Web 监控台填 base_url + api_key |
| 端口 | 8642（hermes API server）、8790（本插件工具桥+监控台），均只绑 127.0.0.1 |

## 四、快速开始

### 1. 构建

```bash
cd plugins/golem_hermes_plugin
go build -o golem_plugin_golem_hermes_plugin.exe .
```

把产物放到 **Golem Host 运行目录**下的 `plugins/`（宿主按工作目录解析，非仓库内 `host/plugins`）。

> 升级已运行中的插件：运行中的 exe 被锁，先 `mv` 旧文件为 `.bak` 再放入新 exe，然后在群里发 `/pm reload golem_hermes_plugin` 热加载。

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

> 修改 hermes 侧内容（SOUL/profile/plugin.yaml/golem_bridge）后需**重启网关**才生效；改 golem 插件本体只需 `/pm reload`。网关是前台进程，窗口被关就没了——挂掉时群聊静默、监控台「概览」探活转红，重新 `hermes gateway` 即可，插件会自动重连。

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

## 五、日常使用（群友视角）

### 聊天

- **@它** 或 **引用它的消息** → 必答；免@白名单群里的消息也视为点名
- 不点名时按 `ambient_reply_rate` 概率旁听插话（同群有冷却）
- 任务型消息（"帮我/查一下/写一…"等关键词）自动走 full 重通道
- 群里发**图片**：被点名时走视觉通道真实看图（宽高/大小元数据从原始报文抢救，不再出现"0B 图"）

### 表情包

- 群里发的表情自动收集、视觉打标入库
- 聊天气氛到位时它自己发表情；你也可以直接要："来个开心的表情"
- 同群任意两张表情间隔 ≥2 分钟（硬节流，冷却中它会如实说"刚发过"）

### 画坊 🎨（生图/改图）

- **下单**：@它 说画什么，如"@小赫 画一只柴犬骑自行车"
- **改图**：先发图片，再 @它 说"把这张图改成毕加索风格"（自动取你刚发的图做素材）；也可引用某张图下单
- 桥接毫秒级接单（不阻塞群聊），画好自动发群；超 15 秒会先提示"画师酝酿中"
- 治理规则：完全相同的需求 → 库存直发 0 元；高度相似（30 分钟窗口内 bigram-Jaccard）→ 拒单；单人每日 3 张、全群日产能可在 Web 调；**@别人让画画的单不接**
- 服务商在 Web「画廊」页配置（enabled/base_url/api_key/model/size），密钥存本地库不进 git

### 史官

- 每日 22:00 自动出刊（四文风轮换），当日群消息量超阈值（默认 500）加发号外
- 出刊记录落库，插件 reload 不会重发同一期
- Web「史官」页可改时刻/阈值、立即出刊、回看旧刊、编辑文风

### 三件套 + 冷场看护（傻吊行为的总开关在会话 ⚙）

| 功能 | 行为 | 开关 |
|---|---|---|
| 🐓 打鸣 | 每日 6:30-9:29 随机时刻喔喔喔（连打三天换另一只鸡） | `crow_enabled` |
| 📓 记仇小本本 | 被说"闭嘴/烦"入账，低频翻旧账 | `grudge_enabled` |
| 🌫️ 突然的自我 | 深夜 22:00-01:59 每群每小时 4% 概率emo一句后自动撤回 | `mood_enabled` |
| 🗣️ 主动冒泡 | 群沉默超过阈值（默认 40 分钟，±25% 抖动）后，agent 带着会话记忆自然接话茬；连续 2 次没人理就闭嘴；每日上限 2 次；01:00-08:00 静默 | `proactive_enabled` + `proactive_min` |

### 人格面具 🎭

- Web「人设」页管理：预置小赫/毒舌老哥/傲娇妹/网抑云/精神小伙，可自建（名字/emoji/一句 brief/完整 prompt）
- 全局默认人设 + 每群 ⚙ 覆盖——不同群可以完全不同的人格和情绪基调
- 主动冒泡、画坊、画像卡同受该群人格影响

### 画像

- 自动积累：成员消息每满 20 条后台增量抽取一次（只处理新增语料，不会越抽越水）
- 群里问它"你记了我什么"必须如实展示（透明原则）；`remember` 工具可对话中即时记忆
- 要画像卡："发一下我的画像卡"/"看看 xx 的人设卡" → 预渲染缓存毫秒级发出（人设+印象+活跃时段+表情倾向）

## 六、配置参考

配置分两级：**config.toml 全局默认**（热更新）+ **群级覆盖**（Web 会话页 ⚙，存 SQLite）。决策时群级覆盖 > 全局默认；`nil` 语义即"跟随全局"。

### 全局（config.toml → `[golem_hermes_plugin.config]`）

| 键 | 默认 | 说明 |
|---|---|---|
| base_url / api_key | — | Hermes API server 地址与密钥 |
| fast/full/vision_profile | fast/full/vision | 通道 profile 名（带图与打标走 vision） |
| bridge_listen / bridge_token | 127.0.0.1:8790 / — | 工具桥+监控台监听与令牌 |
| ambient_reply_rate / ambient_cooldown_sec / min_ambient_len | 0.10 / 300 / 6 | 环境闲聊概率/群冷却/最小长度 |
| free_response_chats | [] | 免@白名单（群wxid） |
| full_keywords | （见源码） | 命中即走任务通道的关键词 |
| context_window / context_max_chars | 60 / 1500 | 旁听上下文条数与注入上限 |
| run_timeout_sec / outbound_slots / max_parallel_sessions | 300 / 4 / 4 | 运行超时、出站并发、并行会话 |
| min_reply_interval_sec / max_reply_chars / max_segments | 3 / 1200 / 3 | 防刷屏三件 |
| daily_token_cap | 0 | 每日 token 上限（超限降级，0=不限） |
| owner_wxids / owner_only_private / owner_bypass_ambient | — | 主人识别追加/私聊门控/主人免@直答 |
| group_sessions_per_user | false | 群会话按成员拆分 |
| session_rotate_min | 60 | Hermes 会话按时间分桶轮换（防转录无限膨胀拖慢响应；0=不轮换） |
| profile_enabled / profile_threshold / profile_batch / profile_inject | true/20/3/true | 群成员画像 |
| crow_enabled / grudge_enabled / mood_enabled | true/true/true | 傻吊三件套全局默认 |
| proactive_min / proactive_daily_cap / proactive_quiet_start / proactive_quiet_end | 40 / 2 / 1 / 8 | 冷场看护全局默认（阈值分钟/日上限/静默时段） |
| chronicle_enabled / chronicle_hour / chronicle_minute | true/22/0 | 史官总开关与出刊时刻 |
| chronicle_extra_threshold | 500 | 号外触发阈值（当日群消息量） |
| chronicle_style_mode / chronicle_fixed_style / chronicle_styles | rotate/太史公体/四预置 | 文风策略与文风池（Web 文风坊可编辑） |
| sticker_enabled / sticker_probability / sticker_cooldown_min / sticker_chat_cooldown_min | true/0.35/10/2 | 表情收集/附带概率/同情绪冷却/同聊天硬节流 |
| imagegen_enabled / imagegen_base_url / imagegen_model / imagegen_size / imagegen_daily_cap / imagegen_workers | false/—/gpt-image-2.5/auto/20/2 | 画坊（密钥建议在 Web 画廊页配置，存本地库） |
| progress_hint_sec | 20 | 任务超时进度提示（0=关） |

### 群级（Web 会话页 ⚙，存 SQLite，即时生效）

`mute`（整群静音，仅主人@穿透）· `free_response` · `ambient_rate` · `ambient_cooldown_sec` · `chronicle` · `session_mode`（shared/per_user）· `crow_enabled` · `grudge_enabled` · `mood_enabled` · **`proactive_enabled` / `proactive_min`**（冷场看护，默认关）· **`imagegen_enabled` / `imagegen_daily_cap`**（画坊）· **`persona`**（人格面具）

成员级（Web 画像详情）：`owner`（动态主人）· `ignored`（屏蔽，决策层全类型忽略）。

## 七、Web 监控台

`http://127.0.0.1:8790/ui`，登录令牌=bridge_token（HttpOnly Cookie，12h；插件 reload 后需重新登录）。

| 视图 | 内容 |
|---|---|
| 概览 | 在途运行/活跃会话/今日token与预算水位/触发率 KPI/画坊与冒泡计数，通道探活，24h 各通道 P50/P95，SSE 每秒刷新 |
| 运行 | 每次运行的触发/通道/耗时/token/状态；行内抽屉展示身份信封、回复全文、工具调用 |
| 决策 | 每条消息的判定路径：响应(@/引用/白名单/环境命中) 或 忽略(冷却/概率未中/预算熔断/去重/屏蔽/静音/@了别人) |
| 会话 | 按聊天统计 + ⚙ 行内群配置（含冷场看护/画坊/人格）+ 上下文查看器 |
| 用量 | 每日 prompt/completion 堆叠趋势、按通道分解 |
| 表情 | 缩略图网格、情绪分布、打标队列、重打标 |
| 画像 | 成员卡片墙 + 角色卡抽屉（画像卡预览/人设/印象时间线/活跃热力/设主人/屏蔽/重跑/删除） |
| 史官 | 编务台（出刊时刻/号外阈值/立即出刊）、旧报纸期卡墙、阅读模态、文风坊 |
| 画廊 | 画坊服务商配置（密钥打码）、成作网格、工单统计 |
| 人设 | 人格面具库 CRUD、全局默认人设指定 |
| 日志 | 插件 slog 环形缓冲实时流 |
| 配置 | 生效配置全览（密钥打码、只读；写操作在各视图行内完成） |

## 八、安全模型

1. **监听仅本机**：bridge/监控台绑 127.0.0.1；开放局域网需自行评估（HTTP 明文 + 令牌单一因素）。
2. **两套鉴权并存**：hermes→插件走 `Authorization: Bearer bridge_token`（工具桥）；浏览器走登录换 HttpOnly Cookie（12h）。
3. **会话绑定防越权**：工具操作只能作用于"活跃窗口内的会话"；其余一律拒绝。agent 无法让消息发到任意 wxid。
4. **主人识别**：协议层 wxid 比对（宿主 Owner 自动并入 + `owner_wxids` 追加），与消息文本自述无关。
5. **密钥分离**：生图服务商密钥存本地 SQLite（Web 配置），不写入 config.toml、不进 git。
6. **隐私**：画像抽取排除敏感字段；出站兜底过滤本机路径；透明原则——任何群友问"你记了我什么"必须如实展示，监控台可删除任意画像。

## 九、目录结构

```
golem_hermes_plugin/
├─ main.go config.go incoming.go dispatch.go engine.go outflow.go bridge.go
│  context.go settings.go store.go metrics.go commands.go
│  fun.go proactive.go          # 傻吊三件套 + 冷场看护
│  imagegen.go                  # 画坊（工单/画手/防复读/交付）
│  persona.go                   # 人格面具
│  profile.go chronicle.go      # 画像/史官
│  imgmeta.go                   # 图片元数据抢救（Raw 报文自解析）
├─ ui.html                      # 监控台单页（go:embed；hermes_data/ui.html 可覆盖热更新）
├─ ui_regression.js             # 前端全视图回归脚本（Console 粘贴执行）
├─ *_test.go                    # 单测与 httptest 集成测试
└─ hermes_side/golem_bridge/    # hermes 侧用户插件（部署到 <HERMES_HOME>/plugins/）
   ├─ plugin.yaml
   └─ __init__.py               # 工具注册 + 微信行为准则注入
```

hermes 侧 agent 可用工具（golem_bridge 注册）：`send_emoji` `send_image`（markdown 渲染）`reply_quote` `get_group_members` `search_chat_history` `get_recent_context` `remember` `get_member_profile` `generate_image`（画坊）`send_member_profile_card`

## 十、测试

```bash
go vet ./...
go test -count=1 ./...          # 单测 + httptest 集成（登录/鉴权/端点/工单/查重/冒泡闸门/画像/设置迁移）
GOLEM_UI_HARNESS=1 go test -run TestUIHarness -v .   # 可选：8791 起带模拟数据的监控台供浏览器手测
# 画坊真机 smoke（打真实生图 API，慎用）：
GOLEM_IMG_SMOKE=1 GOLEM_IMG_BASE=<url> GOLEM_IMG_KEY=<key> go test -run TestSmoke -v .
```

前端改动请在浏览器执行 `ui_regression.js`（全视图断言）后再提交。

## 十一、版本历史

| 版本 | 内容 |
|---|---|
| v0.7.0 | 首个功能完整版：群聊/工具桥/画像/史官/表情库/监控台 |
| v0.8.0 | 画坊：全异步工单制生图/改图（30s+ 同步生成会阻塞群聊，不可接受） |
| v0.8.1 | 画坊防复读：库存直发/相似拒单/单人限购/全局产能四道闸 |
| v0.8.2 | 会话设置静默丢失修复（旧库缺列幂等迁移） |
| v0.8.3 | 真人铁律：禁括号舞台指示，情绪改走表情包 |
| v0.8.4 | 表情风暴修复：同聊天表情硬节流 + 429 量化提示 |
| v0.8.5 | 傻吊三件套修复：fun 引擎接线（曾静默死亡数日） |
| v0.9.0 | 人格面具：人设库 + 每群人格覆盖 |
| v0.9.1 | 画坊拒接 @别人的单（协议级 MentionedOther） |
| v0.9.2 | 会话分桶轮换（39 万 token 转录拖慢响应的治理） |
| v0.9.3 | 画像卡：确定性渲染 + 后台润色 |
| v0.9.4 | 画像密度治理：增量语料 + 变体去重 |
| v0.9.5 | 画像卡缓存：预渲染毫秒发送 |
| v0.9.6 | 史官号外风暴修复：出刊去重落库（reload 不重发） |
| v0.10.0 | 冷场看护：群沉默后带会话记忆主动冒泡（双时钟 + 硬闸族） |
| v0.10.1 | 群图片 0B 修复：从 Raw 报文抢救图片元数据（vision/改图链路恢复） |

## 十二、已知限制

- 引用回复走 App XML（宿主 Send 分支不认 TypeAppQuote），失败自动降级普通文本
- 图片元数据抢救依赖 SDK `Raw` 字段透传原始报文；属性名做宽集合匹配，微信改版导致解析失败时上下文退回中性 `[图片]`（绝不显示误导性 0B）
- 表情打标：无名字表情走视觉模型；provider 无视觉能力则停留待打标（3 次后不再自动重试，可 `/ghp sticker retag`）
- 监控台/工具桥仅本机监听；跨机访问不在本项目安全边界内
- hermes 网关为前台进程：窗口被关即下线（群聊静默、探活转红），需重新 `hermes gateway`

## 十三、常见问题

- **它怎么不回这条消息？** → 监控台「决策」页搜该消息，判定路径一目了然（冷却/概率未中/预算熔断/去重/屏蔽/静音/@了别人…）
- **修改会话设置没生效/又恢复了？** → v0.8.2 已修复旧库缺列导致的静默丢失；确认插件 ≥v0.8.2，Web 保存失败会显式报错
- **主动冒泡太频繁/太害羞？** → 会话 ⚙ 调 `proactive_min`（阈值分钟）与全局 `proactive_daily_cap`；完全关闭就关掉 `proactive_enabled`
- **画坊一直"画师酝酿中"然后失败？** → 画廊页检查服务商配置；日志看 `[画坊]` 前缀；鉴权失败会单独告警 owner
- **打鸣没响** → 打鸣窗口 6:30-9:29，每日时刻由群ID+日期种子固定；确认 ⚙ `crow_enabled` 已开
- **改了 hermes 侧（SOUL/插件）不生效** → 需重启 hermes 网关；改 golem 插件本体只需 `/pm reload golem_hermes_plugin`
- **机器人突然全群沉默** → 概览页探活转红 = hermes 网关挂了，重新 `hermes gateway` 即可，无需 reload 插件
- **`/ghp` 命令** → `/ghp status`（状态）、`/ghp whoami`（主人识别核对）、`/ghp sticker count|retag`（表情库）
