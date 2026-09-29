package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sbgayhub/golem/sdk/chatroom"
	"github.com/sbgayhub/golem/sdk/contact"
	"github.com/sbgayhub/golem/sdk/message"
	"github.com/sbgayhub/golem/sdk/plugin"
)

const (
	pluginName    = "golem_hermes_plugin"
	pluginVersion = "0.9.1"
)

func main() {
	p := newPlugin()
	if err := plugin.RegisterCommand(p.handleStatus); err != nil {
		slog.Error("注册 status 命令失败", "err", err)
		return
	}
	if err := plugin.RegisterCommand(p.handleSticker); err != nil {
		slog.Error("注册 sticker 命令失败", "err", err)
		return
	}
	if err := plugin.RegisterCommand(p.handleWhoami); err != nil {
		slog.Error("注册 whoami 命令失败", "err", err)
		return
	}
	slog.Info("[" + pluginName + "] 启动中", "version", pluginVersion)
	plugin.Start(p)
}

// hermesPlugin 插件主体：群聊业务大脑 + Hermes Agent 引擎客户端 + 微信工具桥
type hermesPlugin struct {
	plugin.ConfigAbility[Config]

	// 由宿主按字段类型自动注入
	message  message.Ability
	contact  contact.Ability
	chatroom chatroom.Ability
	caller   plugin.CallerAbility

	self *contact.SelfInfo
	owner *contact.Contact // 宿主 data/config.toml 的 Owner（机器人主人）

	ownerSet map[string]struct{} // 主人 wxid 精确匹配集（宿主Owner + 配置追加）
	metrics  *metrics            // 可观测性：决策/运行/会话/健康/日志
	profiler *profiler           // 群成员画像后台任务
	chronicle *chronicle        // 群史官
	fun      *funEngine          // 傻吊三件套：打鸣/记仇/突然的自我
	workshop *workshop           // 画坊：生图/改图后台画手

	rings    *ringStore
	eng      *httpEngine
	disp     *dispatcher
	out      *outflow
	store    *store
	tagger   *tagger
	bridge   *bridgeServer
	dataDir  string
	testMode bool // 单元测试下不启动网络服务

	mu      sync.Mutex
	seen    map[int64]time.Time // 消息去重
	ambient map[string]time.Time
}

func newPlugin() *hermesPlugin {
	return &hermesPlugin{
		ConfigAbility: plugin.ConfigAbility[Config]{Config: defaultConfig()},
		seen:          map[int64]time.Time{},
		ambient:       map[string]time.Time{},
	}
}

func (p *hermesPlugin) GetMetadata() *plugin.Metadata {
	return &plugin.Metadata{
		Name:        pluginName,
		Author:      "fCheng",
		Version:     pluginVersion,
		Description: "接入本地 Hermes Agent：自然群聊、双通道分流、微信工具桥、情绪表情包与跨会话记忆",
		Priority:    50,
		Next:        false,
		AlwaysRun:   true,
	}
}

// GetSubscriptions 订阅 "message" 前缀即覆盖全部 message::* 主题（含文本/图片/表情/引用）
func (p *hermesPlugin) GetSubscriptions() []string {
	return []string{"message"}
}

// GetCommands / OnCommand 实现宿主命令接口（/ghp ...）
func (p *hermesPlugin) GetCommands() []string {
	return plugin.CommandCommands()
}

func (p *hermesPlugin) OnCommand(cmd *plugin.Command) (string, error) {
	return plugin.DispatchCommand(cmd)
}

func (p *hermesPlugin) OnLoad() error {
	if p.Config.ContextWindow <= 0 {
		p.Config.ContextWindow = 60
	}
	exe, err := os.Executable()
	if err == nil {
		p.dataDir = filepath.Join(filepath.Dir(exe), "hermes_data")
	} else {
		p.dataDir = "hermes_data"
	}
	if err := os.MkdirAll(filepath.Join(p.dataDir, "sticker_files"), 0o755); err != nil {
		slog.Warn("创建数据目录失败", "dir", p.dataDir, "err", err)
	}
	_ = os.MkdirAll(filepath.Join(p.dataDir, "gallery"), 0o755)

	if p.contact != nil {
		p.self = p.contact.GetSelf()
		p.owner = p.contact.GetOwner()
		p.rebuildOwnerSet()
	}
	slog.Info("[ghp] 登录身份", "username", p.self.GetUsername(), "nickname", p.self.GetNickname())
	slog.Info("[ghp] 主人识别", "host_owner", p.owner.GetUsername(),
		"config_owners", p.Config.OwnerWxIDs, "total", len(p.ownerSet))

	// 可观测性：slog 分流进环形缓冲 + 健康探测循环
	p.metrics = newMetrics()
	slog.SetDefault(slog.New(&telemetryHandler{inner: slog.Default().Handler(), m: p.metrics}))
	go p.metrics.healthLoop(p)

	p.rings = newRingStore(p.Config.ContextWindow)
	p.eng = newEngine(&p.Config)
	p.out = newOutflow(p.message, &p.Config)
	p.store = newStore(filepath.Join(p.dataDir, "state.db"), filepath.Join(p.dataDir, "sticker_files"))
	p.disp = newDispatcher(&p.Config, p.eng, p.out, p.rings, p.store, p.metrics)
	if p.Config.StickerEnabled {
		p.tagger = newTagger(p.store, p.eng, &p.Config)
		go p.tagger.loop()
	}
	if p.Config.ProfileEnabled {
		p.profiler = newProfiler(p)
		go p.profiler.loop()
	}
	if p.Config.ChronicleEnabled {
		p.chronicle = newChronicle(p)
		go p.chronicle.loop()
	}
	if p.imagegenSettings().Enabled {
		p.ensureWorkshop()
	}
	// 傻吊三件套引擎：必须显式启动（v0.7.0 曾漏接此线，打鸣/记仇/深夜emo静默死亡数日）。
	// 引擎恒启动，每 tick 内按会话级开关逐群判定（nil=跟随全局语义）
	p.fun = newFunEngine(p)
	go p.fun.loop()
	if !p.testMode && p.Config.BridgeListen != "" {
		p.bridge = newBridge(p)
		go p.bridge.start()
	}
	return nil
}

func (p *hermesPlugin) OnUnload() error {
	if p.bridge != nil {
		p.bridge.stop()
	}
	if p.disp != nil {
		p.disp.shutdown()
	}
	if p.tagger != nil {
		p.tagger.Stop()
	}
	if p.profiler != nil {
		p.profiler.Stop()
	}
	if p.chronicle != nil {
		p.chronicle.Stop()
	}
	if p.fun != nil {
		p.fun.Stop()
	}
	if p.workshop != nil {
		p.workshop.Stop()
	}
	if p.store != nil {
		p.store.close()
	}
	return nil
}

func (p *hermesPlugin) OnEnable() error  { return nil }
func (p *hermesPlugin) OnDisable() error { return nil }

// rebuildOwnerSet 主人匹配集：宿主 Owner wxid + 配置追加项，全部小写精确比对
func (p *hermesPlugin) rebuildOwnerSet() {
	set := map[string]struct{}{}
	if p.owner != nil && p.owner.GetUsername() != "" {
		set[strings.ToLower(p.owner.GetUsername())] = struct{}{}
	}
	for _, id := range p.Config.OwnerWxIDs {
		if id = strings.ToLower(strings.TrimSpace(id)); id != "" {
			set[id] = struct{}{}
		}
	}
	p.ownerSet = set
}

func (p *hermesPlugin) isOwnerWxid(wxid string) bool {
	if wxid == "" {
		return false
	}
	// 静态集（宿主Owner+config追加） ∪ Web 动态标记（member_flags）
	if _, ok := p.ownerSet[strings.ToLower(wxid)]; ok {
		return true
	}
	return p.store.isMemberOwner(wxid)
}
