# Hermes 表情收藏与自然回复设计

状态：已在 `feat/hermes-sticker-collection` 实现。

## 1. 目标

Hermes 应能理解用户对最近图片或表情的自然语言收藏请求，把用户给出的含义持久化；后续回复时先检索本地收藏，找不到再使用现有外部表情搜索。图片到达时不能自动进入视觉，收藏和回复也不能依赖写死的中文关键词路由。

## 2. Agent 工具流程

### 2.1 收藏

1. Agent 根据自然语言语义判断用户是在请求收藏，并提取用户给出的描述。
2. Agent 调用 `golem_image_search_current_session`，使用可信的发送者、消息 ID、时间和 `is_current_sender` / `is_current_message` 元数据定位目标。
3. Agent 调用 `golem_sticker_collect_current_session(candidate_id, description)`。
4. Golem 校验当前 Relay Run、调用者权限和候选归属，按需下载该候选的真实字节，但不调用视觉。
5. 图片通过 magic-byte、MIME、大小和路径校验后写入收藏库；同一图片按 SHA-256 去重，同图可增加多个描述标签。

“收藏”“记一下”“存一下”等表达只出现在工具语义说明和人格提示中，Go/Python 执行代码不解析这些词，也不根据文本自动选择图片。

### 2.2 回复

1. Agent 认为表情能让当前回复更自然时，先调用 `golem_sticker_library_search(query, limit)`。
2. 本地有结果时，Agent 从 Run 绑定的短期候选中选择一张并调用现有 `golem_sticker_select(candidate_id)`。
3. 本地无结果时，Agent 回退到现有 `golem_sticker_search`，再调用同一个 `golem_sticker_select`。
4. Agent 可以返回 effect-only token 只发表情，也可以继续返回文本。Golem 在同一个 Run 成功事务中按“文本、表情”顺序写入 Outbox。

本地与外部候选共享现有的短期随机 candidate ID、Run/chat scope、物化缓存和选择接口，模型永远拿不到文件路径、远程 URL 或图片原始字节。

## 3. 存储结构

大媒体不存 SQLite BLOB。文件写入独立的 `data_dir/sticker-library/<sha-prefix>/<sha>.<ext>` 内容寻址目录，SQLite V11 保存：

- `sticker_assets`：唯一 SHA-256、MIME、路径、大小和创建时间；
- `sticker_labels`：同图的多个语义描述、可信来源消息/发送者和收藏者审计信息；
- `sticker_terms`：预生成检索片段，主索引为 `(term, sticker_id, weight)`。

收藏写入顺序为“原子安装文件 → 单事务写 asset/label/terms”。数据库失败会删除本次新文件；如果进程在文件安装后、数据库提交前崩溃，下一次收藏相同 SHA 时会验证并接管孤立文件。已有文件损坏时，相同原始字节的再次收藏可原子修复它。

默认磁盘预算为 512 MiB，单张沿用 `max_media_bytes`。收藏库不会为了腾空间静默删除用户收藏；预算耗尽时返回明确错误。视频媒体对象和收藏表情使用独立目录与预算，避免互相驱逐。

## 4. 模糊检索

描述先做 Unicode 小写化、标点归一和空白折叠，再生成带类型前缀的：

- 完整短语和词项；
- Unicode 单字；
- 二元片段；
- 三元片段。

查询只对 `sticker_terms.term` 做等值索引连接并累加权重，不执行全表 `%LIKE%`。这样中文子串、少量错字（例如同一三字词中一个字变化）仍可通过共享二元片段命中，同时延迟随查询片段数和命中候选数增长，而不是随收藏总数线性增长。

SQLite 先返回最多 80 个高分候选；服务端过滤低相关结果，并在最高相关度区间内使用加密随机源洗牌。完全匹配会压过弱相关项，同一描述下的多张表情会随机排列，减少机械重复。选择阶段不额外写数据库，避免回复热路径与 Inbox/Outbox 写事务竞争。

这是低延迟的本地词法模糊检索，不声称提供向量语义搜索。Agent 可通过人格和上下文改变查询措辞；无可靠本地命中时应回退外部搜索。

## 5. 权限和安全

- 默认 `collection_policy = "owner"`：所有交互用户可搜索，只有 owner 可写全局收藏库；可显式改为 `any`。
- 收藏权限在下载图片前校验，未授权请求不会消耗 CDN、内存或磁盘资源。
- 收藏和本地搜索在 ambient Run 与异步 completion 中被阻止；当前实现只服务有活跃、可验证 Relay Run 的交互消息。
- `candidate_id` 必须来自当前 Run 的搜索结果，不能跨聊天或跨 Run 重用。
- 图片像素和图片内文字始终是不可信数据；收藏不调用视觉，识图仍只能由 Agent 显式调用现有 read/inspect 工具。
- 文件读取使用根目录约束、普通文件/非符号链接、大小、SHA-256 和 magic-byte MIME 的重复校验。

## 6. 性能与并发

- SQLite 继续使用 WAL、`busy_timeout` 和短写事务；检索只读，不持有全局写锁。
- 文件按 SHA 分片，避免单目录文件数膨胀。
- 单进程收藏写入由小粒度 mutex 串行化，保证同 SHA 的容量检查、文件安装和数据库去重一致；普通搜索和发送不经过该锁。
- 会话图片物化继续复用现有 single-flight 和 4 路全局下载并发限制；同一候选被识图和收藏时只下载一次。
- 发送继续复用现有 64 MiB 可配置物化 LRU、Run effect 和事务 Outbox，不增加第二条微信发送链路。

## 7. 配置

```toml
[capabilities.sticker]
enabled = true
library_storage_max_bytes = 536870912
collection_policy = "owner"
```

收藏库随现有表情能力启用，不需要额外 Provider。旧配置缺少新字段时自动使用上述默认值。

## 8. 验收场景

1. “收藏刚才某人发的表情，意思是 X”：Agent 先按发送者搜索，再收藏准确候选。
2. “记一下刚才的表情，关键词 X”：不调用视觉也能完成收藏。
3. 同图同描述重复收藏：文件和标签均不重复。
4. 同图不同描述：文件不重复，两个描述都可检索。
5. 近似关键词或单字错误：本地模糊检索可命中。
6. 本地无结果：Agent 调用外部搜索。
7. 纯表情和文字加表情：保持现有 Outbox 顺序与重试语义。
8. 非 owner 收藏：在图片下载前返回拒绝。
9. 旧 V1-V10 数据库加载：自动创建 V11 表和索引，不影响现有 Run/outbox。
10. 并发重复收藏：只产生一个 asset，数据库与磁盘一致。
