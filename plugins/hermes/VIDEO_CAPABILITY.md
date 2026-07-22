# Hermes 微信视频能力

## 1. 目标与边界

视频只在当前微信好友或群聊明确要求视频时发送。Hermes 不得主动推送视频，也不需要理解入站视频内容。

来源是 Golem 中配置的视频 API Provider，例如美女、搞笑、宠物等分类接口。
模型不能提交任意视频 URL、本地路径或视频字节；需要新增来源时，应增加受控
Provider 配置，而不是扩展工具参数。

第一版明确不支持 HLS/m3u8、登录态网页、浏览器 JavaScript 解密、复杂反爬和 yt-dlp。失败时不会伪装成功；如果启用了链接回退，当前回复会发送失败原因和可用的原链接。

## 2. 处理链路

```text
配置的视频 API Provider
  -> 短期候选 ID（绑定当前 Run 和聊天）
  -> 异步准备任务
  -> 安全下载
  -> ffprobe 验证时长、容器和编码
  -> 必要时 ffmpeg 转为 MP4/H.264/AAC
  -> ffmpeg 生成 JPEG 缩略图
  -> 本地不可变媒体对象
  -> Video Effect
  -> Transactional Outbox
  -> message.TypeVideo
  -> 微信
```

Outbox 只保存视频和缩略图的对象 ID，不保存 Base64。SQLite 会在视频 Outbox 插入时建立媒体引用，在发送成功或进入死信后释放引用，因此发送期间不会被缓存清理误删。

## 3. 系统依赖

必须同时安装 `ffmpeg` 和 `ffprobe`，并且 ffmpeg 必须包含 `libx264` 编码器。启用视频能力时，插件会在启动阶段检查这三项，缺少任何一项都会明确拒绝启动视频能力。

OpenCloudOS 先检查系统仓库：

```bash
dnf list --available ffmpeg
dnf install -y ffmpeg
```

如果当前仓库没有该包，应使用运维侧认可的软件源或可信静态构建，不要把未知下载脚本直接通过管道交给 shell。安装后执行：

```bash
command -v ffmpeg
command -v ffprobe
ffmpeg -hide_banner -encoders 2>/dev/null | grep -F libx264
ffprobe -version | head -n 1
```

默认路径是 `/usr/bin/ffmpeg` 和 `/usr/bin/ffprobe`。如果安装在 `/usr/local/bin`，修改 Golem 配置中的对应路径。

## 4. Golem 基础配置

配置位于 Hermes Golem 插件的 TOML 配置区域：

```toml
[hermes.config.capabilities]
environment_file = "data/hermes/capabilities.env"
shared_token_env = "GOLEM_CAPABILITIES_TOKEN"

[hermes.config.capabilities.video]
enabled = true
default_category = "general"
max_candidates = 5
candidate_ttl_seconds = 600
max_source_bytes = 67108864
max_video_bytes = 25165824
max_duration_seconds = 300
max_videos_per_run = 3
prepare_timeout_seconds = 180
download_timeout_seconds = 60
prepare_workers = 2
storage_max_bytes = 2147483648
cache_ttl_hours = 24
ffmpeg_path = "/usr/bin/ffmpeg"
ffprobe_path = "/usr/bin/ffprobe"
media_directory = "data/hermes/media/video"
link_fallback_enabled = true
```

`max_source_bytes` 控制允许下载和处理的源文件，`max_video_bytes` 控制最终发送给微信的 MP4。超过成品上限的兼容 H.264/MP4 也会强制转码。默认成品限制为 `24 MiB`，给微信的约 `25 MB` 边界和容器开销留出余量；成品硬上限为 `25 MiB`，源文件硬上限为 `256 MiB`。

视频上传通常明显慢于文本和表情。应把现有 `[hermes.config.output]` 中的发送超时提高，例如：

```toml
[hermes.config.output]
send_timeout_seconds = 180
```

不要在同一 TOML 文件中重复声明 `[hermes.config.output]`；应修改现有字段。插件 `0.7.1` 起，视频 Outbox 的 lease 会覆盖完整发送超时；任何发送失败、插件重启或 lease 过期都会使该视频直接进入 `dead_letter`，不会再次调用 Host。这个取舍优先避免用户收到重复视频，普通文本和表情仍沿用既有至少一次重试策略。

## 5. Provider 配置

Hermes 不感知各家 API 格式。Provider 负责把不同请求和响应统一为视频候选。

当前生产环境的通用美女视频 Provider 使用以下配置；`video-sending` skill 中的
`xjj_stream` 必须与这里及生产 `plugins/config.toml` 保持一致：

```toml
[[hermes.config.capabilities.video.providers]]
id = "xjj_stream"
categories = ["general", "xjj", "beauty", "美女", "小姐姐"]
priority = 110
endpoint = "https://api.yujn.cn/api/zzxjj.php"
method = "GET"
request_mode = "query"
response_mode = "json"
materialization_mode = "lazy"
timeout_seconds = 15
requests_per_minute = 30
max_response_bytes = 1048576
allowed_media_hosts = ["alimov2.a.kwimgs.com", "txmov2.a.kwimgs.com"]
fallback_url_policy = "none"

[hermes.config.capabilities.video.providers.headers]
User-Agent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 Chrome/131.0.0.0 Safari/537.36"

[hermes.config.capabilities.video.providers.query]
type = "json"

[hermes.config.capabilities.video.providers.response]
success_path = "code"
success_values = ["200"]
items_path = "$"

[hermes.config.capabilities.video.providers.response.url]
path = "data"
transforms = ["trim"]

[hermes.config.capabilities.video.providers.response.title]
path = "title"
transforms = ["trim"]
```

### 5.1 JSON 返回单个或多个 URL

```toml
[[hermes.config.capabilities.video.providers]]
id = "beauty_json"
categories = ["beauty"]
priority = 100
endpoint = "https://api.example.com/videos"
method = "POST"
request_mode = "json"
response_mode = "json"
materialization_mode = "lazy"
timeout_seconds = 15
requests_per_minute = 30
max_response_bytes = 1048576
allowed_media_hosts = ["cdn.example.com"]
fallback_url_policy = "page_url"

[hermes.config.capabilities.video.providers.headers]
Authorization = "Bearer ${env:BEAUTY_VIDEO_API_KEY}"

[hermes.config.capabilities.video.providers.json_body]
limit = 3
enabled = true

[hermes.config.capabilities.video.providers.json_body.filters]
query = "${query}"
category = "${category}"

[hermes.config.capabilities.video.providers.media_headers]
Referer = "https://api.example.com/"

[hermes.config.capabilities.video.providers.response]
success_path = "code"
success_values = ["200"]
items_path = "data.items"
error_path = "message"

[hermes.config.capabilities.video.providers.response.url]
path = "play_url"
transforms = ["trim"]

[hermes.config.capabilities.video.providers.response.title]
path = "title"
transforms = ["trim", "html_unescape"]

[hermes.config.capabilities.video.providers.response.page_url]
path = "share_url"
transforms = ["trim"]

[hermes.config.capabilities.video.providers.response.duration]
path = "duration_ms"
transforms = ["milliseconds_to_seconds"]
```

`json_body` 支持 TOML 原生字符串、数字、布尔、数组和嵌套对象。只有字符串值会展开 `${query}`、`${category}`、`${limit}`、`${page}` 和 `${env:NAME}`。

### 5.2 API 直接返回 MP4 二进制或 302 跳转

```toml
[[hermes.config.capabilities.video.providers]]
id = "funny_binary"
categories = ["funny"]
priority = 90
endpoint = "https://video.example.com/random"
method = "GET"
request_mode = "query"
response_mode = "binary"
materialization_mode = "on_select"
allowed_media_hosts = ["video.example.com", "cdn.example.com"]
fallback_url_policy = "provider_public_url"
public_fallback_url = "https://video.example.com/"
```

`on_select` 表示搜索阶段只创建延迟候选，用户真正选择时才请求随机接口，避免同一随机 API 被调用两次后发送到另一段视频。

### 5.3 纯文本返回 URL

```toml
[[hermes.config.capabilities.video.providers]]
id = "animal_text_url"
categories = ["animals"]
endpoint = "https://api.example.com/random-animal"
method = "GET"
request_mode = "query"
response_mode = "text_url"
materialization_mode = "lazy"
allowed_media_hosts = ["media.example.com"]

[hermes.config.capabilities.video.providers.response.url]
transforms = ["trim", "markdown_link_target"]
```

### 5.4 同一接口可能返回 JSON 错误或视频

使用显式 `auto`：

```toml
response_mode = "auto"
materialization_mode = "on_select"

[hermes.config.capabilities.video.providers.response]
success_path = "code"
success_values = ["200"]
error_path = "message"
```

`auto` 会根据 Content-Type 和内容特征选择 JSON、文本 URL 或二进制解析器，并在日志和错误中暴露真实失败，不会静默吞掉业务错误。

## 6. 凭据

Provider 凭据写入 `capabilities.environment_file`：

```dotenv
GOLEM_CAPABILITIES_TOKEN=replace-with-a-random-shared-token
BEAUTY_VIDEO_API_KEY=replace-with-provider-key
```

敏感 Header、Query、Form 或 JSON 字段必须使用 `${env:NAME}`，不能直接写在 TOML 或 endpoint URL 中。

## 7. Hermes 插件配置

部署完整插件目录，而不是只替换 `__init__.py`：

```bash
mkdir -p /root/.hermes/plugins/golem_sticker_capabilities
cp -a /path/to/golem/plugins/hermes/hermes_plugin/golem_sticker_capabilities/. \
  /root/.hermes/plugins/golem_sticker_capabilities/
```

Hermes `.env`：

```dotenv
GOLEM_CAPABILITIES_URL=http://127.0.0.1:8789
GOLEM_CAPABILITIES_TOKEN=replace-with-the-same-shared-token
GOLEM_CAPABILITIES_TIMEOUT_SECONDS=15
GOLEM_ASYNC_DELIVERY_ENABLED=true
GOLEM_VIDEO_ENABLED=true
GOLEM_VIDEO_PREPARE_TIMEOUT_SECONDS=210
```

视频工具沿用现有 `golem_stickers` toolset，因此 Hermes `config.yaml` 保持：

```yaml
plugins:
  enabled:
    - golem-sticker-capabilities

platform_toolsets:
  relay:
    - web
    - file
    - skills
    - golem_stickers
    - no_mcp
```

三个视频发送工具：

- `golem_video_search`：按分类调用已配置视频 API。
- `golem_video_select`：启动准备任务、轮询完成状态并暂存一条视频回复。
- `golem_video_attach`：后台子代理或 Golem 绑定的定时任务使用，准备视频后直接创建持久化 Outbox。

`golem_video_select` 可以按用户要求重复调用。每次成功选择形成独立有序 Effect，最终一次 Run 可以产生文本、表情和多条视频 Outbox。

后台子代理使用 `golem_video_search` 和 `golem_video_attach`。Search 会自动使用
delivery ticket 绑定的聊天作用域；Attach 只接受不透明 `candidate_id`，
`invocation_id` 由 Hermes middleware 注入。Provider URL、媒体字节、Receiver、
chat ID 和本地路径都不会出现在模型参数中。只有视频处理完成、媒体对象落盘且
Direct Output 事务成功创建 Outbox 后，Attach 才返回 `queued=true`。

定时任务创建时，Golem 会持久化原始微信会话绑定。每次触发时 Hermes 插件注入
`profile`、`job_id` 和唯一 `delivery_id`，定时任务主 Agent 或其子代理可直接调用
`golem_video_search` 和 `golem_video_attach`。Go 端只根据数据库绑定选择 Receiver，
模型不能填写 `chat_id`。同一轮的每次 Attach 使用 Hermes `tool_call_id` 幂等创建
独立 Outbox；已经创建视频 Outbox 时，普通 cron completion 文本会被抑制。
Hermes 使用平台工具白名单时，需要在 `platform_toolsets.cron` 中加入
`golem_stickers`。

私聊和群聊都必须使用 `deliver="origin"` 创建或更新任务。不要把目标写成
`relay:chatroom:...`；该形式会走 Hermes 的实时 Relay Adapter，当前回合结束后没有
持久通道。Golem Hermes 插件 `0.7.4` 同时校验 Hermes 的群共享 session key 和
群成员隔离 session key，并继续要求 `chat_id`、当前发送者、触发消息和活动 Run
全部匹配，因此 `group_sessions_per_user` 的取值不会再导致群聊注册返回 HTTP 409。

## 8. 加载与验证

安装系统依赖、替换 Golem 插件二进制并修改配置后，重新加载 Hermes 插件即可，不要求重启整个 Golem Host：

```text
/pm load hermes
```

替换 Hermes Python 插件或修改 Hermes `.env` 后，需要重启 Hermes Gateway。启动前确认只有一个 Hermes 插件实例监听能力端口：

```bash
ps -ef | grep '[g]olem_plugin_hermes'
ss -lntp | grep ':8789'
```

Hermes 插件自测：

```bash
HERMES_PROJECT="$(hermes version | sed -n 's/^Project: //p')"
HERMES_PY="${HERMES_PROJECT%/lib/python*/site-packages}/bin/python"
"$HERMES_PY" /root/.hermes/plugins/golem_sticker_capabilities/test_plugin.py
"$HERMES_PY" /root/.hermes/plugins/golem_sticker_capabilities/test_video_tools.py
"$HERMES_PY" /root/.hermes/plugins/golem_sticker_capabilities/test_video_async_tools.py
```

最终发送仍由 Golem Host 的 `message.Ability.Send` 完成，不使用 `cdn.Ability` 预上传。
