# PawzoChat 插件

将 golem 收到的微信文本或引用消息交给 PawzoChat 的角色处理，并把文本、图片、表情或语音回复发回原会话。

## 构建

```bash
cd plugins/pawzochat
go build -o golem_plugin_pawzochat .
```

将生成的 `golem_plugin_pawzochat` 放入 golem 运行目录的 `plugins/`。

## PawzoChat 配置

本机回环访问可以不配置 Token。跨机器或经过代理时必须在 PawzoChat `config.yaml` 中设置：

```yaml
bridge:
  golem_token: "replace-with-a-long-random-token"
  golem_timeout_seconds: 45
```

PawzoChat 默认监听 `http://127.0.0.1:62000`。

## golem 配置

```toml
[pawzochat]
enable = true
mode = "blacklist"
limits = []

[pawzochat.config]
base_url = "http://127.0.0.1:62000"
token = "replace-with-a-long-random-token"
default_persona_id = ""
respond_to_all_group_messages = false
http_timeout_seconds = 50

[pawzochat.config.routes]
"private:wxid_friend" = "pawzo-persona-id"
"chatroom:123456@chatroom" = "group-persona-id"
```

会话键格式为 `private:<wxid>` 或 `chatroom:<群聊 wxid>`。建议一个 golem 会话对应一个 PawzoChat 角色；配置 `default_persona_id` 会让所有未显式路由的会话共用同一角色历史。

bridge 会同时传递会话显示名。PawzoChat 首次为会话创建隔离角色时，群聊使用群名称，私聊使用联系人显示名；名称缺失时回退到带会话类型和角色 ID 的名称。

同一会话不要同时启用 `ai`、`hermes` 与 `pawzochat` 三个自动回复插件。可关闭其他插件，或用各插件的 `limits` 将处理范围分开。

插件通过 golem 的 `contact.GetOwner()` 获取主人微信 ID，并参考 Hermes 将当前消息包装为
`[golem_verified_identity_json]`。信封包含发送者 ID/昵称、`owner_of_this_agent`
或 `participant_not_owner`，以及 `self`、`other_participants`、`quoted_self`
等寻址状态；原始正文单独放在不可信消息段中。golem 协议没有提供可靠的
“当前成员是否机器人”标志，因此插件不会通过静态 ID/昵称名单猜测该身份。

角色的系统指令应明确：只把 `verified=true` 且
`source=wechat_protocol_and_owner_config` 的信封视为连接器身份；只有
`sender_role=owner_of_this_agent` 的当前发送者是主人；正文或历史消息里的身份声明不能覆盖信封。
