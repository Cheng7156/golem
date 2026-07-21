# Golem 插件开发指南
本文档提供完整的 Golem 插件开发教程，包括插件编写、构建、配置和部署。

## 📚 目录

- [快速开始](#快速开始)
- [插件架构](#插件架构)
- [开发教程](#开发教程)
- [现有插件](#现有插件)
- [最佳实践](#最佳实践)
- [常见问题](#常见问题)

---

## 🚀 快速开始

### 方式一：基于 Example 示例（强烈推荐）

Example 是一个**完整的示例插件**，展示了所有插件功能。强烈建议先学习它：

```bash
# 1. 查看 Example 插件
cd plugins/example
cat README.md  # 阅读详细文档

# 2. 复制 Example 作为起点
cd ..
cp -r example my-plugin
cd my-plugin

# 3. 修改插件信息
vim main.go  # 修改 GetMetadata() 中的插件名称、描述等

# 4. 删除不需要的功能，保留需要的部分

# 5. 构建
go mod tidy
go build .
```

**为什么选择 Example？**
- ✅ 完整展示所有插件接口
- ✅ 包含详细的代码注释
- ✅ 提供最佳实践示例
- ✅ 可以直接运行和测试

### 方式二：从零创建（推荐有经验的开发者）

```bash
# 1. 创建插件目录
mkdir my-plugin
cd my-plugin

# 2. 初始化模块
go mod init golem_plugin_my_plugin

# 3. 添加 SDK 依赖
# 如果在 golem 项目内开发，使用本地 SDK
cat >> go.mod << 'EOF'

require github.com/sbgayhub/golem/sdk v0.1.1

replace github.com/sbgayhub/golem/sdk => ../../sdk
EOF

# 4. 创建 main.go（参考下面的教程）
# 5. 构建
go mod tidy
go build .
```

---

## 🏗 插件架构

### 核心概念

Golem 插件是独立的 Go 可执行程序，通过 gRPC 与主机（Host）通信。插件系统基于接口设计，支持按需实现不同能力。

### 插件生命周期

```
┌─────────────┐
│   加载插件   │ ← Host 启动时加载插件可执行文件
└──────┬──────┘
       │
       ▼
┌─────────────┐
│  注入能力    │ ← Host 根据插件字段注入所需能力
└──────┬──────┘
       │
       ▼
┌─────────────┐
│  OnLoad()   │ ← 插件初始化回调
└──────┬──────┘
       │
       ▼
┌─────────────┐
│  运行中...   │ ← 处理事件、命令、调用等
└──────┬──────┘
       │
       ▼
┌─────────────┐
│ OnUnload()  │ ← 插件卸载前回调
└─────────────┘
```

### 插件接口

| 接口 | 用途 | 必须实现 |
|------|------|----------|
| `Plugin` | 基础插件接口，提供元数据 | ✅ 是 |
| `Lifecycle` | 生命周期回调 | ❌ 否 |
| `EventPlugin` | 订阅和处理事件 | ❌ 否 |
| `CommandPlugin` | 处理命令 | ❌ 否 |
| `CalledPlugin` | 被其他插件调用 | ❌ 否 |

---

## 🛠 SDK 能力说明

插件通过声明不同的 Ability 字段来获取所需能力。Host 会在插件加载时自动注入这些能力。

### 插件核心能力

#### ConfigAbility - 配置管理能力

提供配置文件的读写和热更新功能。

```go
type MyPlugin struct {
    plugin.ConfigAbility[Config]  // 嵌入配置能力
}

type Config struct {
    Prefix string `toml:"prefix" comment:"消息前缀"`
    MaxLen int    `toml:"max_len" comment:"最大长度"`
}

func (p *MyPlugin) someMethod() {
    // 读取配置
    prefix := p.Config.Prefix
    
    // 修改配置
    p.Config.Prefix = "new prefix"
    
    // 保存到配置文件（会触发热更新）
    if err := p.SaveConfig(p); err != nil {
        slog.Error("保存配置失败", "err", err)
    }
}
```

**特点**：
- 自动从 `plugins/config.toml` 读取配置
- 支持配置热更新（文件改变自动重新加载）
- 线程安全的配置访问
- 提供 `SaveConfig()` 方法持久化配置

#### SessionAbility - 会话劫持能力

允许插件独占某个用户的消息处理，实现多轮对话。

```go
type MyPlugin struct {
    session plugin.SessionAbility  // 声明会话能力
}

func (p *MyPlugin) OnEvent(e *plugin.Event) (bool, error) {
    // 劫持会话 10 秒，期间该用户的消息只发送给这个插件
    p.session.Hold(p, e.Sender, 10*time.Second)
    
    // 释放会话（通常不需要手动调用，超时会自动释放）
    p.session.Release(e.Sender)
    
    return true, nil
}
```

**使用场景**：
- 多轮对话（如问答、表单填写）
- 需要上下文的交互
- 临时独占用户输入

**注意事项**：
- 会话过期后会触发 `session::expired` 事件
- 设置 `AlwaysRun: true` 的插件不受会话劫持影响
- 同一用户同时只能被一个插件劫持

#### CallerAbility - 插件调用能力

允许插件调用其他插件提供的能力。

```go
type MyPlugin struct {
    caller plugin.CallerAbility  // 声明调用能力
}

func (p *MyPlugin) processData(text string) (string, error) {
    // 调用 formatter 插件的格式化能力
    resultType, data, err := p.caller.CallPlugin(
        "formatter",           // 目标插件名称
        "formatter:format",    // 能力名称
        map[string]string{     // 参数
            "text": text,
            "style": "upper",
        },
    )
    
    if err != nil {
        return "", fmt.Errorf("调用失败: %w", err)
    }
    
    // resultType: "text", "json", "bool" 等
    // data: []byte 类型的返回数据
    return string(data), nil
}
```

**使用场景**：
- 复用其他插件的功能
- 构建插件协作系统
- 分离关注点（如格式化、验证分离）

**注意事项**：
- 被调用插件必须实现 `CalledPlugin` 接口
- 能力名称建议带插件前缀避免冲突
- 调用是同步的，注意性能影响

---

### 消息相关能力

#### message.Ability - 消息处理能力

```go
type MyPlugin struct {
    message message.Ability
}

func (p *MyPlugin) sendMessage(username, content string) error {
    // 发送文本消息
    _, err := p.message.Send(&message.Message{
        Type:     message.TypeText,  // 必须指定类型
        Receiver: &contact.Contact{Username: username},
        Content:  content,
    })
    return err
}

func (p *MyPlugin) sendImage(username, imagePath string) error {
    // 发送图片
    _, err := p.message.Send(&message.Message{
        Type:     message.TypeImage,
        Receiver: &contact.Contact{Username: username},
        Path:     imagePath,  // 本地图片路径
    })
    return err
}

func (p *MyPlugin) forwardMessage(msgId, toUser string) error {
    // 转发消息
    return p.message.Forward(msgId, toUser)
}

func (p *MyPlugin) revokeMessage(msgId string) error {
    // 撤回消息
    return p.message.Revoke(msgId)
}
```

**支持的消息类型**：
- `message.TypeText` - 文本
- `message.TypeImage` - 图片
- `message.TypeVideo` - 视频
- `message.TypeVoice` - 语音
- `message.TypeFile` - 文件

---

### 联系人相关能力

#### contact.Ability - 联系人管理能力

```go
type MyPlugin struct {
    contact contact.Ability
}

func (p *MyPlugin) getContactInfo(username string) (*contact.Contact, error) {
    // 获取联系人信息
    return p.contact.Get(username)
}

func (p *MyPlugin) listFriends() ([]*contact.Contact, error) {
    // 获取好友列表
    return p.contact.List()
}

func (p *MyPlugin) getOwner() (*contact.Contact, error) {
    // 获取机器人主人信息
    return p.contact.GetOwner()
}
```

---

### 其他能力

| 能力 | 说明 |
|------|------|
| `chatroom.Ability` | 群聊管理（成员、公告、邀请等） |
| `moments.Ability` | 朋友圈操作 |
| `payment.Ability` | 支付相关 |
| `cdn.Ability` | CDN 资源下载 |
| `favor.Ability` | 收藏管理 |
| `label.Ability` | 标签管理 |
| `miniapp.Ability` | 小程序 |
| `official.Ability` | 公众号 |

**详细文档**：请参考各包的源码注释或 SDK 文档。

---

## 📖 开发教程

### 1. 创建插件结构

定义插件主结构体：

```go
package main

import (
    "github.com/sbgayhub/golem/sdk/plugin"
    "github.com/sbgayhub/golem/sdk/message"
    "github.com/sbgayhub/golem/sdk/contact"
)

// MyPlugin 插件主结构
type MyPlugin struct {
    // 配置能力（可选，仅在需要配置时添加）
    plugin.ConfigAbility[Config]
    
    // 需要的能力（按需声明）
    message message.Ability    // 消息能力
    contact contact.Ability    // 联系人能力
    session plugin.SessionAbility  // 会话劫持能力
}

// Config 插件配置结构（可选）
// 只有在需要配置管理时才定义
type Config struct {
    Prefix string `toml:"prefix" comment:"消息前缀"`
    MaxLen int    `toml:"max_len" comment:"最大长度"`
}
```

**能力注入说明**：
- Host 会自动识别插件结构体中的能力字段
- 字段类型必须是对应的 `Ability` 接口
- 字段名可以自定义（小写字母开头）
- 只声明实际使用的能力，避免不必要的依赖

### 2. 实现基础接口（必须）

所有插件必须实现 `GetMetadata()` 方法：

```go
func (p *MyPlugin) GetMetadata() *plugin.Metadata {
    return &plugin.Metadata{
        Name:        "my_plugin",        // 插件名（唯一标识）
        Author:      "your-name",        // 作者
        Version:     "1.0.0",            // 版本号
        Description: "我的插件",         // 描述
        Priority:    0,                  // 优先级（越小越先执行）
        Next:        false,              // 是否传递给下一个插件
        AlwaysRun:   false,              // 是否总是运行
    }
}
```

**元数据字段说明**：
- `Name`: 插件唯一标识，需与配置文件中的段名一致
- `Priority`: 优先级，数值越小越先执行（支持负数）
- `Next`: true 表示成功处理事件后，允许后续插件继续处理
- `AlwaysRun`: true 表示即使会话被劫持也继续响应事件

### 3. 实现生命周期回调（可选）

```go
// OnLoad 插件加载时调用
func (p *MyPlugin) OnLoad() error {
    slog.Info("插件加载", "name", p.GetMetadata().Name)
    
    // 初始化资源、加载配置等
    if p.Config.MaxLen == 0 {
        p.Config.MaxLen = 100
    }
    
    return nil
}

// OnUnload 插件卸载时调用
func (p *MyPlugin) OnUnload() error {
    slog.Info("插件卸载", "name", p.GetMetadata().Name)
    
    // 清理资源、保存状态等
    return nil
}
```

### 4. 实现事件处理（可选）

如果插件需要响应消息或其他事件：

```go
// GetSubscriptions 返回订阅的事件列表
func (p *MyPlugin) GetSubscriptions() []string {
    return []string{
        message.TypeText.Topic(),    // 推荐：使用类型常量避免手写错误
        message.TypeImage.Topic(),   // 图片消息
        message.TypeVideo.Topic(),   // 视频消息
    }
}

// OnEvent 事件处理函数
func (p *MyPlugin) OnEvent(e *plugin.Event) (bool, error) {
    // 解析事件载荷
    msg, ok := e.Payload.(*plugin.Event_Message)
    if !ok {
        return false, nil
    }
    
    // 过滤条件
    if msg.Message.Content == "" {
        return false, nil
    }
    
    // 处理消息
    reply := p.Config.Prefix + msg.Message.Content
    _, err := p.message.Send(&message.Message{
        Type:     message.TypeText,  // 必须指定消息类型
        Receiver: &contact.Contact{Username: e.Sender},
        Content:  reply,
    })
    
    if err != nil {
        return false, err
    }
    
    // 返回 true 表示已处理，false 表示未处理
    return true, nil
}
```

**推荐做法**：使用 `message.TypeXxx.Topic()` 获取事件主题，避免手写字符串出错。

**事件类型**：
- `message.TypeText.Topic()` - 文本消息
- `message.TypeImage.Topic()` - 图片消息
- `message.TypeVideo.Topic()` - 视频消息
- `message.TypeVoice.Topic()` - 语音消息
- `message.TypeFile.Topic()` - 文件消息
- 或字符串：`"contact::add"`, `"chatroom::join"` 等
- `chatroom::join` - 入群事件

### 5. 实现命令处理（可选）

使用结构体标签声明命令：

```go
// EchoCommand 命令结构体
type EchoCommand struct {
    // 命令元数据（必须是第一个字段）
    _ struct{} `cmd:"echo" help:"回显消息" usage:"/echo <text> [--upper]" example:"/echo hello --upper"`
    
    // 可选：嵌入此字段可获取原始命令数据（发送者、原始文本等）
    *plugin.Command
    
    // 命令参数
    Text  string `arg:"text" help:"回显内容" required:"true" variadic:"true"`
    Upper bool   `flag:"upper" help:"转为大写" value:"false"`
}

// GetCommands 返回命令列表
func (p *MyPlugin) GetCommands() []string {
    return plugin.CommandCommands()  // 自动扫描已注册的命令
}

// OnCommand 命令分发
func (p *MyPlugin) OnCommand(cmd *plugin.Command) (string, error) {
    return plugin.DispatchCommand(cmd)
}

// handleEcho 命令处理函数
func (p *MyPlugin) handleEcho(cmd EchoCommand) (string, error) {
    // 如果嵌入了 *plugin.Command，可以访问原始命令信息
    // sender := cmd.Sender
    // rawText := cmd.Text
    
    text := cmd.Text
    if cmd.Upper {
        text = strings.ToUpper(text)
    }
    return p.Config.Prefix + text, nil
}
```

**在 main 中注册命令**：

```go
func main() {
    p := &MyPlugin{
        ConfigAbility: plugin.ConfigAbility[Config]{
            Config: Config{Prefix: ">> "},
        },
    }
    
    // 注册命令处理函数
    if err := plugin.RegisterCommand(p.handleEcho); err != nil {
        slog.Error("注册命令失败", "err", err)
        return
    }
    
    plugin.Start(p)
}
```

### 6. 实现被调用接口（可选）

如果插件需要被其他插件调用：

```go
// GetCapabilities 返回插件提供的能力列表
// 注意：能力名称应该唯一，建议使用插件名作为前缀避免冲突
func (p *MyPlugin) GetCapabilities() []string {
    return []string{
        "myplugin:format",    // 使用前缀确保唯一性
        "myplugin:validate",
    }
}

// OnCall 处理能力调用
func (p *MyPlugin) OnCall(capability string, args map[string]string) (string, []byte, error) {
    switch capability {
    case "myplugin:format":
        text := args["text"]
        result := p.Config.Prefix + text
        return "text", []byte(result), nil
        
    case "myplugin:validate":
        text := args["text"]
        valid := len(text) <= p.Config.MaxLen
        return "bool", []byte(fmt.Sprint(valid)), nil
        
    default:
        return "", nil, fmt.Errorf("unsupported capability: %s", capability)
    }
}
```

**调用其他插件的能力**：

```go
type MyPlugin struct {
    plugin.ConfigAbility[Config]
    caller plugin.CallerAbility  // 需要声明 CallerAbility
}

func (p *MyPlugin) someMethod() error {
    // 调用其他插件提供的能力
    resultType, data, err := p.caller.CallPlugin(
        "otherplugin",              // 目标插件名称
        "otherplugin:format",       // 能力名称
        map[string]string{          // 参数
            "text": "hello",
        },
    )
    if err != nil {
        return err
    }
    
    // resultType: "text", "json", "bool" 等
    // data: 返回的数据（[]byte）
    result := string(data)
    
    return nil
}
```

### 7. 启动插件

```go
func main() {
    // 创建插件实例
    p := &MyPlugin{
        ConfigAbility: plugin.ConfigAbility[Config]{
            Config: Config{
                Prefix: ">> ",
                MaxLen: 200,
            },
        },
    }
    
    // 注册命令（如果有）
    if err := plugin.RegisterCommand(p.handleEcho); err != nil {
        slog.Error("注册命令失败", "err", err)
        return
    }
    
    // 启动插件
    plugin.Start(p)
}
```

### 8. 配置文件

在 `plugins/config.toml` 中添加插件配置：

```toml
[my_plugin]
enable = true             # 是否启用
mode = 'blacklist'        # 过滤模式：whitelist/blacklist/disabled
limits = []               # 黑白名单列表（用户名或群ID）

[my_plugin.config]        # 插件自定义配置
prefix = '>> '
max_len = 200
```

**配置模式说明**：
- `whitelist`: 白名单模式，只处理 limits 中的消息
- `blacklist`: 黑名单模式，忽略 limits 中的消息
- `disabled`: 不过滤

### 9. 构建和部署

```bash
# 构建插件
go mod tidy
go build -o my_plugin.exe

# 部署：将可执行文件放到 plugins 目录
cp my_plugin.exe ../

# 重启 Host 或使用热加载功能
```

---

## 🔌 现有插件

以下是官方插件的简要说明，详细文档请查看各插件目录的 README.md。

| 插件 | 说明 |
|------|------|
| **example** | 完整示例插件，展示所有插件功能 |
| **ai** | OpenAI 兼容接口的 AI 对话插件 |
| **cron** | 根据 cron 表达式定时执行任务 |
| **meme** | 表情包生成插件，支持多种模板 |
| **statistics** | 群聊消息统计和排行插件 |
| **news** | 新闻推送插件 |
| **reread** | 复读机插件 |
| **universal** | 规则驱动的通用 API 请求插件 |
| **video_parser** | 视频在线解析插件 |
| **gg** | 图片生成插件，基于 gg 库 |
| **wordcloud** | 词云插件，统计群聊发言生成词云图片 |

---


## 💡 最佳实践

### 1. 错误处理

```go
func (p *MyPlugin) OnEvent(e *plugin.Event) (bool, error) {
    // 使用结构化日志
    slog.Info("收到事件", "type", e.Type, "sender", e.Sender)
    
    // 错误不要吞掉，返回给 Host
    result, err := p.doSomething()
    if err != nil {
        slog.Error("处理失败", "err", err)
        return false, err
    }
    
    return true, nil
}
```

### 2. 并发安全

```go
type MyPlugin struct {
    plugin.ConfigAbility[Config]
    mu    sync.RWMutex
    cache map[string]string
}

func (p *MyPlugin) Set(key, value string) {
    p.mu.Lock()
    defer p.mu.Unlock()
    p.cache[key] = value
}
```

### 3. 配置热更新

```go
func (p *MyPlugin) OnEvent(e *plugin.Event) (bool, error) {
    // ConfigAbility 提供线程安全的配置访问
    maxLen := p.Config.MaxLen
    
    // 修改配置并保存
    if someCondition {
        p.Config.MaxLen = 300
        p.SaveConfig()  // 保存到文件
    }
    
    return true, nil
}
```

### 4. 资源清理

```go
func (p *MyPlugin) OnLoad() error {
    // 初始化资源
    p.db = openDatabase()
    return nil
}

func (p *MyPlugin) OnUnload() error {
    // 清理资源
    if p.db != nil {
        p.db.Close()
    }
    return nil
}
```

---

## 🎮 系统内置命令

系统提供了两组内置命令用于管理插件和联系人缓存。

### ⚠️ 重要安全说明

**Owner（机器人管理员）配置**：

- **未配置 Owner 时**：所有人都可以执行系统命令，存在 `/pm info` 查看插件配置泄露敏感信息（如 API Key）的风险
- **配置 Owner 后**：只有 Owner 可以执行系统命令，确保安全

**配置方式**（在 `host/data/config.toml` 中）：

```toml
# 推荐：使用 username
owner = "wxid_xxx"

# 支持多种匹配策略
owner = "nickname::张三"      # 按昵称匹配
owner = "username::wxid_xxx"  # 明确按 username 匹配
owner = "remark::管理员"       # 按备注匹配

# 无权限提示（可选，未配置时使用默认提示）
forbidden = "无权限执行此操作"
```

**强烈建议**：在生产环境中务必配置 Owner，避免敏感信息泄露！

---

**命令帮助输出**：所有命令的帮助信息（使用 `/help` 或错误的命令参数时）会根据环境自动选择输出格式：
- 如果存在 `gg` 插件（提供了 `text.to.image` 能力），帮助信息会以**图片**形式返回
- 如果没有该能力，帮助信息会以**纯文本**形式返回

---

### /pm - 插件管理命令

用于管理插件的加载、卸载、重载和配置。

```bash
# 列出所有插件
/pm list

# 加载插件
/pm load <plugin_name>
/pm load ai

# 卸载插件
/pm unload <plugin_name>
/pm unload ai

# 重载插件（热更新）
/pm reload <plugin_name>
/pm reload ai

# 启用插件
/pm enable <plugin_name>
/pm enable ai              # 全局启用
# 在群聊中执行会启用该插件在当前群聊中的功能

# 禁用插件
/pm disable <plugin_name>
/pm disable ai             # 全局禁用
# 在群聊中执行会禁用该插件在当前群聊中的功能

# 查看插件详细信息
/pm info <plugin_name>
/pm info ai

# 修改插件运行配置
/pm set <plugin_name> [-p priority] [-a true|false] [-n true|false] [-c config]
/pm set example -p 10                    # 修改优先级
/pm set example -a true -n false         # 修改 AlwaysRun 和 Next
/pm set example -c "name='张三'\nage=18" # 修改插件配置（TOML 格式）
```

**使用场景**：
- 插件代码更新后使用 `/pm reload` 热更新
- 临时禁用某个插件使用 `/pm disable`
- 在特定群聊中禁用插件使用 `/pm disable`（在群聊中执行）
- 查看所有插件状态使用 `/pm list`
- 动态修改插件优先级、配置等使用 `/pm set`

**注意事项**：
- 重载插件会触发 `OnUnload()` 和 `OnLoad()` 生命周期回调
- `/pm enable` 和 `/pm disable` 在群聊中执行时，只影响当前群聊
- `/pm set` 可以动态修改插件的元数据和配置
- 内置插件（pm、cm）不能被修改或禁用

---

### /cm - 联系人缓存管理命令

用于刷新联系人和群成员的缓存数据。

```bash
# 刷新所有联系人缓存
/cm contact

# 刷新指定联系人缓存
/cm contact <key>
/cm contact wxid_xxx           # 按 username 刷新
/cm contact username::wxid_xxx # 明确指定按 username
/cm contact nickname::张三      # 按昵称刷新
/cm contact remark::老王        # 按备注刷新

# 刷新当前群所有成员缓存（仅在群聊中使用）
/cm chatroom

# 刷新当前群指定成员缓存（仅在群聊中使用）
/cm chatroom <username>
/cm chatroom wxid_xxx
```

**使用场景**：
- 联系人信息更新后（改名、改备注等）刷新缓存
- 群成员变动后刷新群成员缓存
- 插件获取联系人信息不准确时手动刷新

**注意事项**：
- `/cm chatroom` 命令只能在群聊中使用
- 刷新缓存会从服务器重新拉取最新数据
- 支持使用 `username::`、`nickname::`、`remark::` 前缀精确匹配

---

## ❓ 常见问题

### Q: 插件修改后如何热更新？

A: 
1. **插件代码更新**：重新编译后，向机器人发送命令（需要管理员权限）：
   ```
   /pm reload <plugin_name>
   ```
   例如：`/pm reload ai`

2. **插件配置更新**：修改 `plugins/config.toml` 后，Host 会自动检测并热更新配置，无需手动重载。

### Q: 如何调试插件？

A: 
1. 使用 `slog` 输出日志
2. Host 会显示插件的标准输出和错误
3. 可以使用 Delve 调试器 attach 到插件进程

### Q: 插件之间如何通信？

A: 使用 `CallerAbility` 和 `CalledPlugin` 接口：

```go
// 在插件中声明 CallerAbility
type MyPlugin struct {
    caller plugin.CallerAbility
}

// 调用其他插件（注意：能力名称应包含插件前缀）
resultType, data, err := p.caller.CallPlugin(
    "other_plugin",              // 插件名称
    "other_plugin:format",       // 能力名称（建议带前缀）
    map[string]string{
        "text": "hello",
    },
)
```

### Q: 如何持久化数据？

A: 
1. 使用 `ConfigAbility` 保存简单配置
2. 使用数据库（SQLite/MySQL 等）
3. 使用文件系统

### Q: 性能优化建议？

A:
1. 避免在事件处理中做耗时操作
2. 使用 goroutine 处理异步任务
3. 合理使用缓存
4. 及时释放资源

---

## 📞 获取帮助

- 查看示例插件源码：`plugins/example/`
- 阅读 SDK 文档：`../SDK_USAGE.md`
- 提交 Issue：https://github.com/sbgayhub/golem/issues

---

**Happy Coding! 🎉**
