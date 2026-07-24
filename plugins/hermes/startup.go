package main

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	"golem_plugin_hermes/internal/agent"
	"golem_plugin_hermes/internal/app"
	"golem_plugin_hermes/internal/config"
	"golem_plugin_hermes/internal/domain"
	"golem_plugin_hermes/internal/execution"
	"golem_plugin_hermes/internal/ingress"
	"golem_plugin_hermes/internal/mediaobject"
	"golem_plugin_hermes/internal/observation"
	"golem_plugin_hermes/internal/output"
	"golem_plugin_hermes/internal/routing"
	sqlitestore "golem_plugin_hermes/internal/store/sqlite"
	"golem_plugin_hermes/internal/tool"
)

type pluginRuntime struct {
	config       config.Config
	databasePath string
	store        *sqlitestore.Store
	manager      *config.Manager
	processor    *ingress.Processor
	engine       agent.Engine
	kernel       *app.Kernel
	recovered    domain.RecoveryResult
}

type runtimeAssembly struct {
	plugin        *HermesPlugin
	config        config.Config
	store         *sqlitestore.Store
	manager       *config.Manager
	engine        agent.Engine
	runtimeRunner app.Runner
	runWake       chan struct{}
	outputWake    chan struct{}
	mediaObjects  mediaobject.Reader
	mediaResolver golemMediaResolver
}

type runtimeBuildInput struct {
	ctx          context.Context
	config       config.Config
	manager      *config.Manager
	store        *sqlitestore.Store
	databasePath string
}

func (p *HermesPlugin) buildRuntime() (*pluginRuntime, error) {
	cfg, err := config.Normalize(p.Config)
	if err != nil {
		return nil, fmt.Errorf("规范化 Hermes 配置: %w", err)
	}
	manager, err := config.NewManager(cfg)
	if err != nil {
		return nil, fmt.Errorf("创建 Hermes Config Manager: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	databasePath := filepath.Join(cfg.DataDir, "hermes.db")
	store, err := sqlitestore.Open(ctx, databasePath)
	if err != nil {
		return nil, fmt.Errorf("启动 Hermes Store: %w", err)
	}
	return p.assembleRuntime(runtimeBuildInput{
		ctx: ctx, config: cfg, manager: manager, store: store, databasePath: databasePath,
	})
}

func (p *HermesPlugin) assembleRuntime(input runtimeBuildInput) (*pluginRuntime, error) {
	runWake := make(chan struct{}, 1)
	outputWake := make(chan struct{}, 1)
	mediaResolver := golemMediaResolver{message: p.message, cdn: p.cdn}
	built, err := buildGateway(input.config, input.store, outputWake, mediaResolver)
	if err != nil {
		_ = input.store.Close()
		return nil, err
	}
	engine, err := agent.NewRuntime(built.gateway)
	if err != nil {
		_ = built.gateway.Close(input.ctx)
		_ = input.store.Close()
		return nil, fmt.Errorf("create Hermes Agent Runtime: %w", err)
	}
	assembly := runtimeAssembly{
		plugin: p, config: input.config, store: input.store, manager: input.manager, engine: engine,
		runtimeRunner: built.runner, runWake: runWake, outputWake: outputWake,
		mediaObjects: built.mediaObjects, mediaResolver: mediaResolver,
	}
	return assembly.start(input.ctx, input.databasePath)
}

type gatewayBuild struct {
	gateway      agent.Gateway
	runner       app.Runner
	mediaObjects mediaobject.Reader
}

func buildGateway(
	cfg config.Config,
	store *sqlitestore.Store,
	outputWake chan<- struct{},
	mediaResolver golemMediaResolver,
) (gatewayBuild, error) {
	switch cfg.Agent.Mode {
	case "relay":
		return buildRelayGateway(cfg, store, outputWake, mediaResolver)
	case "http":
		gateway, err := agent.NewHTTPEngine(agent.HTTPConfig{
			BaseURL: cfg.Agent.BaseURL,
			APIKey:  cfg.Agent.APIKey,
			Model:   cfg.Agent.Model,
		}, nil)
		if err != nil {
			return gatewayBuild{}, fmt.Errorf("create Hermes HTTP compatibility gateway: %w", err)
		}
		return gatewayBuild{gateway: gateway}, nil
	default:
		return gatewayBuild{}, fmt.Errorf("unsupported Hermes Agent mode %q", cfg.Agent.Mode)
	}
}

func buildRelayGateway(
	cfg config.Config,
	store *sqlitestore.Store,
	outputWake chan<- struct{},
	mediaResolver golemMediaResolver,
) (gatewayBuild, error) {
	capabilities, err := buildRelayCapabilityBundle(
		cfg,
		store,
	)
	if err != nil {
		return gatewayBuild{}, err
	}
	relay, err := agent.NewRelayGateway(agent.RelayConfig{
		ListenAddress:        cfg.Agent.RelayListen,
		Path:                 cfg.Agent.RelayPath,
		GatewayID:            cfg.Agent.RelayGatewayID,
		SharedSecret:         cfg.Agent.RelaySharedSecret,
		SilenceRulesFile:     cfg.Agent.SilenceRulesFile,
		CapabilityToken:      capabilities.token,
		Stickers:             capabilities.stickers,
		Videos:               capabilities.videos,
		VideoLinkFallback:    cfg.Capabilities.Video.LinkFallbackEnabled,
		AsyncDelivery:        asyncDeliveryCapability(cfg, store),
		AsyncVideoJobs:       store,
		CronDelivery:         cronDeliveryCapability(cfg, store),
		AsyncDeliveryWake:    func() { signalWake(outputWake) },
		MediaDirectory:       filepath.Join(cfg.DataDir, "media"),
		ImageContext:         store,
		ImageResolver:        mediaResolver,
		RunResults:           store,
		ObservationV2Enabled: cfg.Context.Mode == "full",
		RecentRawMessages:    cfg.Context.RecentRawMessages,
		MaxProjectionTokens:  cfg.Context.MaxProjectionTokens,
	})
	if err != nil {
		return gatewayBuild{}, fmt.Errorf("create Hermes Gateway relay: %w", err)
	}
	return gatewayBuild{gateway: relay, runner: relay, mediaObjects: capabilities.mediaObjects}, nil
}

func (a runtimeAssembly) start(ctx context.Context, databasePath string) (*pluginRuntime, error) {
	runners, processor, err := a.createRunners()
	if err != nil {
		a.cleanup()
		return nil, err
	}
	kernel, err := app.NewKernel(a.store, runners...)
	if err != nil {
		a.cleanup()
		return nil, fmt.Errorf("创建 Hermes Kernel: %w", err)
	}
	recovered, err := kernel.Start(ctx, time.Now())
	if err != nil {
		a.cleanup()
		return nil, fmt.Errorf("恢复 Hermes 状态: %w", err)
	}
	return &pluginRuntime{
		config: a.config, databasePath: databasePath, store: a.store, manager: a.manager,
		processor: processor, engine: a.engine, kernel: kernel, recovered: recovered,
	}, nil
}

func (a runtimeAssembly) createRunners() ([]app.Runner, *ingress.Processor, error) {
	broker, err := tool.NewBroker(a.config.Scheduler.ToolWorkers, 1<<20, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("create Hermes capability broker: %w", err)
	}
	social, err := buildSocialDecider(a.config, a.store)
	if err != nil {
		return nil, nil, fmt.Errorf("create Hermes SocialDecider: %w", err)
	}
	router, err := routing.NewRulesRouter(a.manager.Current, social, a.store)
	if err != nil {
		return nil, nil, fmt.Errorf("创建 Hermes Router: %w", err)
	}
	admission, err := execution.NewRunAdmissionCoordinator(a.store, a.engine, a.manager.Current)
	if err != nil {
		return nil, nil, fmt.Errorf("create Hermes Run admission coordinator: %w", err)
	}
	processor, err := ingress.NewProcessor(
		a.store, router,
		time.Duration(a.config.Ingress.ReorderWindowMilliseconds)*time.Millisecond,
		a.runWake, admission,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("创建 Hermes Ingress Processor: %w", err)
	}
	runners := []app.Runner{processor}
	if observer, ok := a.engine.(agent.ObservationGateway); ok {
		dispatcher, observeErr := observation.NewDispatcher(a.store, observer, nil)
		if observeErr != nil {
			return nil, nil, observeErr
		}
		dispatcher.SetEnabled(func() bool {
			current := a.manager.Current()
			return current != nil && current.Context.Mode == "full"
		})
		runners = append(runners, dispatcher)
	}
	runners, err = a.appendDispatchers(runners)
	if err != nil {
		return nil, nil, err
	}
	if a.runtimeRunner != nil {
		runners = append(runners, a.runtimeRunner)
	}
	return a.appendWorkers(runners, broker, processor)
}

func (a runtimeAssembly) appendDispatchers(runners []app.Runner) ([]app.Runner, error) {
	sender := newGolemSender(
		a.plugin.message, a.config.Scheduler.InteractiveWorkers, a.mediaObjects,
	)
	throttler := output.NewThrottler()
	for range a.config.Output.Workers {
		dispatcher, err := output.NewDispatcher(a.store, sender, a.manager.Current, a.outputWake, throttler)
		if err != nil {
			return nil, fmt.Errorf("create Hermes output dispatcher: %w", err)
		}
		runners = append(runners, dispatcher)
	}
	return runners, nil
}

func (a runtimeAssembly) appendWorkers(
	runners []app.Runner,
	broker *tool.Broker,
	processor *ingress.Processor,
) ([]app.Runner, *ingress.Processor, error) {
	control, err := execution.NewControlWorker(a.store, a.engine, a.runWake, a.outputWake)
	if err != nil {
		return nil, nil, err
	}
	runners = append(runners, control)
	runners, err = a.appendLaneWorkers(runners, broker, a.mediaResolver, domain.LaneInteractive)
	if err != nil {
		return nil, nil, err
	}
	runners, err = a.appendLaneWorkers(runners, broker, a.mediaResolver, domain.LaneJob)
	return runners, processor, err
}

func (a runtimeAssembly) appendLaneWorkers(
	runners []app.Runner,
	broker *tool.Broker,
	mediaResolver golemMediaResolver,
	lane domain.Lane,
) ([]app.Runner, error) {
	count := a.config.Scheduler.InteractiveWorkers
	start := 0
	if lane == domain.LaneJob {
		count = a.config.Scheduler.JobWorkers
		start = a.config.Scheduler.InteractiveWorkers
	}
	for index := range count {
		var options []execution.WorkerOption
		if lane == domain.LaneInteractive &&
			a.config.Scheduler.RunAdmissionMode != string(domain.RunAdmissionOff) &&
			index < a.config.Scheduler.InteractiveReservedWorkers {
			options = append(options, execution.WithTriggerFilter(domain.TriggerExplicit))
		}
		worker, err := execution.NewWorker(
			start+index, a.store, a.engine, broker, mediaResolver, lane,
			a.manager.Current, a.runWake, a.outputWake, options...,
		)
		if err != nil {
			return nil, err
		}
		runners = append(runners, worker)
	}
	return runners, nil
}

func (a runtimeAssembly) cleanup() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = a.engine.Close(ctx)
	_ = a.store.Close()
}

func (p *HermesPlugin) installRuntime(runtime *pluginRuntime) {
	p.Config = runtime.config
	p.store = runtime.store
	p.kernel = runtime.kernel
	p.config = runtime.manager
	p.processor = runtime.processor
	p.engine = runtime.engine
	p.refreshIdentity()
	slog.Info("[hermes] 内核存储已启动",
		"database", runtime.databasePath,
		"runs_recovered", runtime.recovered.RunsRecovered,
		"outbox_recovered", runtime.recovered.OutboxRecovered,
	)
}
