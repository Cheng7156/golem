package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"golem_plugin_hermes/internal/agent"
	"golem_plugin_hermes/internal/app"
	"golem_plugin_hermes/internal/config"
	"golem_plugin_hermes/internal/ingress"
	sqlitestore "golem_plugin_hermes/internal/store/sqlite"

	"github.com/sbgayhub/golem/sdk/cdn"
	"github.com/sbgayhub/golem/sdk/contact"
	"github.com/sbgayhub/golem/sdk/message"
	"github.com/sbgayhub/golem/sdk/plugin"
)

type HermesPlugin struct {
	plugin.ConfigAbility[config.Config]
	message message.Ability
	cdn     cdn.Ability
	contact contact.Ability

	lifecycleMu sync.Mutex
	store       *sqlitestore.Store
	kernel      *app.Kernel
	config      *config.Manager
	processor   *ingress.Processor
	engine      agent.Engine
	commands    *plugin.CommandRegistry
	commandErr  error

	identityMu        sync.RWMutex
	identityRefreshMu sync.Mutex
	self              *contact.SelfInfo
	ownerID           string
	identityUpdatedAt time.Time
}

func newHermesPlugin() *HermesPlugin {
	p := &HermesPlugin{
		ConfigAbility: plugin.ConfigAbility[config.Config]{
			Config: config.Default(),
		},
		commands: plugin.NewCommandRegistry(),
	}
	p.commandErr = plugin.RegisterCommandTo(p.commands, p.handleHermesCommand)
	return p
}

func (p *HermesPlugin) GetMetadata() *plugin.Metadata {
	return &plugin.Metadata{
		Name:        "hermes",
		Author:      "Golem Team",
		Version:     "0.9.0",
		Description: "事件驱动、可恢复、全异步的 Hermes 对话内核",
		Priority:    1<<31 - 2,
		Next:        false,
		AlwaysRun:   false,
	}
}

func (p *HermesPlugin) OnLoad() error {
	return p.start()
}

func (p *HermesPlugin) OnEnable() error {
	return p.start()
}

func (p *HermesPlugin) OnUnload() error {
	return p.stop()
}

func (p *HermesPlugin) OnDisable() error {
	return p.stop()
}

func (p *HermesPlugin) OnConfigChange() error {
	cfg, err := config.Normalize(p.Config)
	if err != nil {
		return fmt.Errorf("规范化热重载 Hermes 配置: %w", err)
	}
	p.lifecycleMu.Lock()
	manager := p.config
	if manager == nil {
		p.lifecycleMu.Unlock()
		return nil
	}
	current := manager.Current()
	if current != nil && socialDeciderStaticConfigChanged(current.Routing, cfg.Routing) {
		p.lifecycleMu.Unlock()
		return errors.New("routing SocialDecider 的 endpoint/model/key/context 配置已变化，需要 reload Hermes 插件")
	}
	if current != nil && contextStaticConfigChanged(current.Context, cfg.Context) {
		p.lifecycleMu.Unlock()
		return errors.New("context 配置已变化，需要 reload Hermes 插件以重新协商 Relay 协议")
	}
	if current != nil && current.Scheduler != cfg.Scheduler {
		p.lifecycleMu.Unlock()
		return errors.New("scheduler 配置已变化，需要 reload Hermes 插件以重建 worker 与准入策略")
	}
	snapshot, err := manager.Publish(cfg)
	p.lifecycleMu.Unlock()
	if err != nil {
		return fmt.Errorf("发布热重载 Hermes 配置: %w", err)
	}
	slog.Info("[hermes] 运行时配置已生效",
		"version", snapshot.Version,
		"social_mode", snapshot.Routing.SocialMode,
		"sample_rate", snapshot.Routing.SampleRate,
	)
	return nil
}

func contextStaticConfigChanged(current, next config.ContextConfig) bool {
	return current != next
}

func socialDeciderStaticConfigChanged(current, next config.RoutingConfig) bool {
	return current.DecisionBaseURL != next.DecisionBaseURL ||
		current.DecisionModel != next.DecisionModel ||
		current.DecisionAPIKeyEnv != next.DecisionAPIKeyEnv ||
		current.DecisionEnvironmentFile != next.DecisionEnvironmentFile ||
		current.DecisionContextMessages != next.DecisionContextMessages ||
		current.DecisionMinConfidence != next.DecisionMinConfidence
}

func (p *HermesPlugin) start() error {
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()
	if p.store != nil {
		return nil
	}
	if p.commandErr != nil {
		return fmt.Errorf("注册 Hermes 命令: %w", p.commandErr)
	}
	runtime, err := p.buildRuntime()
	if err != nil {
		return err
	}
	p.installRuntime(runtime)
	return nil
}

func (p *HermesPlugin) stop() error {
	p.lifecycleMu.Lock()
	store := p.store
	kernel := p.kernel
	engine := p.engine
	p.store = nil
	p.kernel = nil
	p.config = nil
	p.processor = nil
	p.engine = nil
	p.lifecycleMu.Unlock()
	if store == nil {
		return nil
	}
	timeout := time.Duration(p.Config.ShutdownGraceSeconds) * time.Second
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	kernelErr := kernel.Stop(ctx)
	engineErr := engine.Close(ctx)
	storeErr := store.Close()
	if err := errors.Join(kernelErr, engineErr, storeErr); err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("停止 Hermes Kernel: %w", err)
	}
	return nil
}

func main() {
	p := newHermesPlugin()
	if p.commandErr != nil {
		slog.Error("[hermes] 注册命令失败", "err", p.commandErr)
		return
	}
	plugin.Start(p)
}
