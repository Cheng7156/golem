# Hermes 任意 URL 视频抓取与异步发送设计

状态：已实现首版

## 1. 用例

用户在微信发送：

```text
@Hermes http://example.com/video-api 把这个视频发给我
```

入口可能直接返回视频流、返回包含视频地址的纯文本，或返回结构未知的 JSON。下载、转码和微信上传可能持续数分钟，不能占用父 Agent 的会话 Turn。

## 2. 执行时序

```text
微信父 Turn
  -> delegate_task(background=true)
  -> 父 Turn 立即确认并结束

后台子代理（持有 Golem async delivery ticket）
  -> golem_video_fetch(url)
  -> POST /capabilities/v1/async-delivery/videos/inspect
  -> Go 校验 ticket、会话绑定和公共网络地址
  -> GET 入口并识别 video / text URL / JSON

video 或 text URL
  -> POST /capabilities/v1/async-delivery/videos/send-url

JSON
  -> 返回脱敏 document + 排序后的候选 path/url
  -> 子代理判断真实视频字段
  -> golem_video_fetch(url, media_url)
  -> POST /capabilities/v1/async-delivery/videos/send-url

Go 后台视频任务
  -> 注册绑定 ticket scope 的短期 direct_url candidate
  -> 公网安全下载 -> ffprobe -> 必要时 ffmpeg -> JPEG 缩略图
  -> 不可变媒体对象
  -> CommitAsyncDirectOutput(kind=video)
  -> Transactional Outbox -> 微信
```

`send-url` 返回 `202 + job_id`，Python 工具短轮询 status。轮询只发生在 Hermes 后台 child 中；Go 下载任务使用独立 runtime goroutine，Relay HTTP 请求不被长下载占住。

## 3. JSON 选择协议

首个工具调用不会让 Go 猜一个结构未知 JSON 的业务语义。Go 做确定性工作：

- 响应体有最大字节数、最大嵌套深度和字符串长度；
- `token`、`secret`、`password`、`cookie`、`authorization`、API key 等字段脱敏；
- 收集绝对 URL 和明确的相对 URL，记录 JSON path；
- 按字段名（video/media/play/download/source/src/stream/file）和视频扩展名评分；
- 最多返回 32 个候选。

子代理可以结合字段名和文档结构选择精确 URL，再把它交回 Go。最终地址仍必须通过 Go 的协议、DNS/IP、重定向、大小、格式和时长校验；模型的判断不是安全授权。

## 4. 权限与绑定

- `golem_video_fetch` 没有 async binding 时明确失败，因此父 Agent 不能同步执行慢任务。
- ticket 绑定 profile、producer epoch、delegation、Hermes session、Relay session、chat、parent Run 和真实 Golem receiver。
- 工具参数不包含微信目标、ticket 或 invocation ID；这些值来自受信 ContextVar。
- 每次实际发送使用 Hermes tool call ID 作为 invocation ID；重试返回同一 Outbox，参数变化的重用被拒绝。
- ticket 被 revoke/abandon 后不能新增发送；已经提交的 Outbox 不被撤回。

## 5. 网络与媒体安全

- 只接受绝对 HTTP(S) URL，拒绝 URL 凭据和本地文件路径。
- HTTP 是否允许由 `url_fetch_allow_http` 控制；HTTPS 始终允许。
- 自定义 Dialer 在每次连接时解析 DNS，只连接公网 IP，拒绝 loopback、私网、链路本地、未指定、多播地址，覆盖 DNS rebinding 的连接时校验。
- 最多跟随 5 次合法 HTTP(S) 重定向；跨源时移除敏感请求头。
- JSON/文本探测、源视频、成品视频、时长、磁盘、超时和并发均有独立上限。
- 只接受受支持的视频容器魔数；HLS/m3u8、登录态网页、JavaScript 解密和 yt-dlp 不在首版范围。

## 6. 失败语义

- 探测错误返回后台 child，由 child 给原会话提交一条明确错误文本。
- 下载/处理失败把异步视频 job 标为 failed，不伪装为 queued。
- 只有 `CommitAsyncDirectOutput` 成功才返回 `queued=true`；随后 synthetic completion 静默关闭 ticket，避免再发一条工具摘要。
- 插件重启或视频发送 lease 过期沿用现有视频 Outbox 防重复策略，不自动重试不确定的视频上传。
