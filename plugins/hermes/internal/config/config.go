package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
)

func Normalize(value Config) (Config, error) {
	defaults := Default()
	if err := normalizeBase(&value, defaults); err != nil {
		return Config{}, err
	}
	if err := normalizeRouting(&value.Routing, defaults.Routing); err != nil {
		return Config{}, err
	}
	if err := normalizeContext(&value.Context, defaults.Context); err != nil {
		return Config{}, err
	}
	if err := normalizeScheduler(&value.Scheduler, defaults.Scheduler); err != nil {
		return Config{}, err
	}
	if err := normalizeAgent(&value.Agent, defaults.Agent); err != nil {
		return Config{}, err
	}
	if value.Scheduler.RunAdmissionMode == "active" && value.Agent.Mode != "relay" {
		return Config{}, errors.New("scheduler.run_admission_mode=active requires agent.mode=relay")
	}
	if err := normalizeOutput(&value.Output, defaults.Output); err != nil {
		return Config{}, err
	}
	if err := normalizeCapabilities(&value.Capabilities, defaults.Capabilities); err != nil {
		return Config{}, err
	}
	if (value.Capabilities.Sticker.Enabled || value.Capabilities.Video.Enabled || value.Agent.AsyncDeliveryEnabled) && value.Agent.Mode != "relay" {
		return Config{}, errors.New("Hermes 扩展能力只支持 agent.mode = relay")
	}
	return value, nil
}

func normalizeContext(value *ContextConfig, defaults ContextConfig) error {
	value.Mode = strings.ToLower(strings.TrimSpace(value.Mode))
	if value.Mode == "" {
		value.Mode = defaults.Mode
	}
	switch value.Mode {
	case "legacy_shadow", "full", "none":
	default:
		return fmt.Errorf("不支持的 context.mode: %s", value.Mode)
	}
	value.Backfill = strings.ToLower(strings.TrimSpace(value.Backfill))
	if value.Backfill == "" {
		value.Backfill = defaults.Backfill
	}
	if value.Backfill != "from_now" {
		return fmt.Errorf("context.backfill=%s 尚未安全实现；当前仅支持 from_now", value.Backfill)
	}
	if value.RecentRawMessages <= 0 {
		value.RecentRawMessages = defaults.RecentRawMessages
	}
	if value.RecentRawMessages > 200 {
		return errors.New("context.recent_raw_messages 不能大于 200")
	}
	if value.MaxProjectionTokens <= 0 {
		value.MaxProjectionTokens = defaults.MaxProjectionTokens
	}
	if value.MaxProjectionTokens > 65536 {
		return errors.New("context.max_projection_tokens 不能大于 65536")
	}
	if value.BarrierWaitMilliseconds <= 0 {
		value.BarrierWaitMilliseconds = defaults.BarrierWaitMilliseconds
	}
	if value.BarrierWaitMilliseconds > 10000 {
		return errors.New("context.barrier_wait_milliseconds 不能大于 10000")
	}
	return nil
}

func normalizeBase(value *Config, defaults Config) error {
	value.DataDir = strings.TrimSpace(value.DataDir)
	if value.DataDir == "" {
		value.DataDir = defaults.DataDir
	}
	value.DataDir = filepath.Clean(value.DataDir)
	if value.DataDir == "." {
		return errors.New("data_dir 不能是当前工作目录")
	}
	if value.ShutdownGraceSeconds <= 0 {
		value.ShutdownGraceSeconds = defaults.ShutdownGraceSeconds
	}
	value.BotNames = normalizeStrings(value.BotNames)
	if value.Ingress.ReorderWindowMilliseconds < 0 {
		return errors.New("ingress.reorder_window_milliseconds 不能为负数")
	}
	if value.Ingress.DurableAcceptTimeoutMilliseconds <= 0 {
		value.Ingress.DurableAcceptTimeoutMilliseconds = defaults.Ingress.DurableAcceptTimeoutMilliseconds
	}
	return nil
}

func normalizeRouting(value *RoutingConfig, defaults RoutingConfig) error {
	value.SocialMode = strings.ToLower(strings.TrimSpace(value.SocialMode))
	switch value.SocialMode {
	case "observe", "rules", "mentions", "hybrid", "agent":
	default:
		return fmt.Errorf("不支持的 routing.social_mode: %s", value.SocialMode)
	}
	if value.SampleRate < 0 || value.SampleRate > 1 {
		return errors.New("routing.sample_rate 必须在 0 到 1 之间")
	}
	if value.DecisionTimeoutMilliseconds <= 0 {
		value.DecisionTimeoutMilliseconds = defaults.DecisionTimeoutMilliseconds
	}
	if value.DecisionContextMessages <= 0 {
		value.DecisionContextMessages = defaults.DecisionContextMessages
	}
	if value.DecisionContextMessages > 30 {
		return errors.New("routing.decision_context_messages 不能大于 30")
	}
	if value.DecisionMinConfidence <= 0 {
		value.DecisionMinConfidence = defaults.DecisionMinConfidence
	}
	if value.DecisionMinConfidence <= 0 || value.DecisionMinConfidence > 1 {
		return errors.New("routing.decision_min_confidence 必须大于 0 且不超过 1")
	}
	value.DecisionBaseURL = strings.TrimRight(strings.TrimSpace(value.DecisionBaseURL), "/")
	if value.DecisionBaseURL != "" {
		parsed, err := url.Parse(value.DecisionBaseURL)
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
			return errors.New("routing.decision_base_url 必须是有效 HTTPS URL")
		}
	}
	value.DecisionModel = strings.TrimSpace(value.DecisionModel)
	value.DecisionAPIKeyEnv = strings.TrimSpace(value.DecisionAPIKeyEnv)
	if value.DecisionAPIKeyEnv == "" {
		value.DecisionAPIKeyEnv = defaults.DecisionAPIKeyEnv
	}
	if !environmentNamePattern.MatchString(value.DecisionAPIKeyEnv) {
		return errors.New("routing.decision_api_key_env 不是有效的环境变量名")
	}
	value.DecisionEnvironmentFile = strings.TrimSpace(value.DecisionEnvironmentFile)
	if value.DecisionEnvironmentFile != "" {
		value.DecisionEnvironmentFile = filepath.Clean(value.DecisionEnvironmentFile)
		if value.DecisionEnvironmentFile == "." {
			return errors.New("routing.decision_environment_file 不能是目录")
		}
	}
	if value.SocialMode == "hybrid" && (value.DecisionBaseURL == "" || value.DecisionModel == "") {
		return errors.New("routing.social_mode=hybrid 需要 decision_base_url 和 decision_model")
	}
	if value.OrdinaryFreshnessSeconds <= 0 {
		value.OrdinaryFreshnessSeconds = defaults.OrdinaryFreshnessSeconds
	}
	if value.CoalesceWindowMilliseconds <= 0 {
		value.CoalesceWindowMilliseconds = defaults.CoalesceWindowMilliseconds
	}
	if value.AmbientCooldownSeconds <= 0 {
		value.AmbientCooldownSeconds = defaults.AmbientCooldownSeconds
	}
	if value.AmbientWindowSeconds <= 0 {
		value.AmbientWindowSeconds = defaults.AmbientWindowSeconds
	}
	if value.AmbientMaxReplies <= 0 {
		value.AmbientMaxReplies = defaults.AmbientMaxReplies
	}
	value.AutomatedSpeakerNames = normalizeStrings(value.AutomatedSpeakerNames)
	value.AutomatedSpeakerIDs = normalizeStrings(value.AutomatedSpeakerIDs)
	return nil
}

func normalizeScheduler(value *SchedulerConfig, defaults SchedulerConfig) error {
	value.RunAdmissionMode = strings.ToLower(strings.TrimSpace(value.RunAdmissionMode))
	if value.RunAdmissionMode == "" {
		value.RunAdmissionMode = defaults.RunAdmissionMode
	}
	switch value.RunAdmissionMode {
	case "off", "queued", "active":
	default:
		return fmt.Errorf("不支持的 scheduler.run_admission_mode: %s", value.RunAdmissionMode)
	}
	if value.RouterWorkers <= 0 {
		value.RouterWorkers = defaults.RouterWorkers
	}
	if value.InteractiveWorkers <= 0 {
		value.InteractiveWorkers = defaults.InteractiveWorkers
	}
	if value.InteractiveReservedWorkers < 0 || value.InteractiveReservedWorkers >= value.InteractiveWorkers {
		return errors.New("interactive_reserved_workers 必须小于 interactive_workers")
	}
	if value.JobWorkers <= 0 {
		value.JobWorkers = defaults.JobWorkers
	}
	if value.ToolWorkers <= 0 {
		value.ToolWorkers = defaults.ToolWorkers
	}
	if value.MaxActiveSessions <= 0 {
		value.MaxActiveSessions = defaults.MaxActiveSessions
	}
	return nil
}

func normalizeAgent(value *AgentConfig, defaults AgentConfig) error {
	value.Mode = strings.ToLower(strings.TrimSpace(value.Mode))
	if value.Mode == "" {
		value.Mode = defaults.Mode
	}
	switch value.Mode {
	case "relay", "http":
	default:
		return fmt.Errorf("不支持的 agent.mode: %s", value.Mode)
	}
	value.RelayListen = strings.TrimSpace(value.RelayListen)
	if value.RelayListen == "" {
		value.RelayListen = defaults.RelayListen
	}
	if _, _, err := net.SplitHostPort(value.RelayListen); err != nil {
		return fmt.Errorf("agent.relay_listen 无效: %w", err)
	}
	value.RelayPath = "/" + strings.Trim(strings.TrimSpace(value.RelayPath), "/")
	if value.RelayPath == "/" {
		value.RelayPath = defaults.RelayPath
	}
	value.RelaySessionNamespace = strings.TrimSpace(value.RelaySessionNamespace)
	value.SilenceRulesFile = strings.TrimSpace(value.SilenceRulesFile)
	if value.SilenceRulesFile != "" {
		value.SilenceRulesFile = filepath.Clean(value.SilenceRulesFile)
		if value.SilenceRulesFile == "." {
			return errors.New("agent.silence_rules_file 不能是目录")
		}
	}
	value.RelayGatewayID = strings.TrimSpace(value.RelayGatewayID)
	value.RelaySharedSecret = strings.TrimSpace(value.RelaySharedSecret)
	if (value.RelayGatewayID == "") != (value.RelaySharedSecret == "") {
		return errors.New("agent.relay_gateway_id 与 relay_shared_secret 必须同时配置")
	}
	if value.Mode == "relay" && value.RelaySharedSecret == "" && !isLoopbackAddress(value.RelayListen) {
		return errors.New("未配置鉴权的 agent relay 只能监听 loopback 地址")
	}
	value.BaseURL = strings.TrimRight(strings.TrimSpace(value.BaseURL), "/")
	value.APIKey = strings.TrimSpace(value.APIKey)
	value.Model = strings.TrimSpace(value.Model)
	value.SystemPrompt = strings.TrimSpace(value.SystemPrompt)
	if value.Mode == "http" && value.Model == "" {
		return errors.New("agent.model 不能为空")
	}
	if value.Mode == "http" && value.BaseURL == "" {
		return errors.New("agent.mode=http 时 base_url 不能为空")
	}
	if value.TimeoutSeconds <= 0 {
		value.TimeoutSeconds = defaults.TimeoutSeconds
	}
	return nil
}

func normalizeOutput(value *OutputConfig, defaults OutputConfig) error {
	value.DeliverySemantics = strings.ToLower(strings.TrimSpace(value.DeliverySemantics))
	if value.DeliverySemantics == "" {
		value.DeliverySemantics = defaults.DeliverySemantics
	}
	if value.DeliverySemantics != "at_least_once" {
		return errors.New("Hermes 1.0 只支持 output.delivery_semantics=at_least_once")
	}
	if value.Workers <= 0 {
		value.Workers = defaults.Workers
	}
	if value.MaxAttempts <= 0 {
		value.MaxAttempts = defaults.MaxAttempts
	}
	if value.AmbiguousMaxAttempts <= 0 {
		value.AmbiguousMaxAttempts = defaults.AmbiguousMaxAttempts
	}
	if value.SendTimeoutSeconds <= 0 {
		value.SendTimeoutSeconds = defaults.SendTimeoutSeconds
	}
	if value.RetryMinSeconds <= 0 {
		value.RetryMinSeconds = defaults.RetryMinSeconds
	}
	if value.RetryMaxSeconds < value.RetryMinSeconds {
		return errors.New("output.retry_max_seconds 不能小于 retry_min_seconds")
	}
	if value.SendIntervalMilliseconds < 0 || value.SendJitterMilliseconds < 0 {
		return errors.New("发送间隔和抖动不能为负数")
	}
	return nil
}

func normalizeCapabilities(value *CapabilityConfig, defaults CapabilityConfig) error {
	value.EnvironmentFile = strings.TrimSpace(value.EnvironmentFile)
	if value.EnvironmentFile != "" {
		value.EnvironmentFile = filepath.Clean(value.EnvironmentFile)
		if value.EnvironmentFile == "." {
			return errors.New("capabilities.environment_file 不能是目录")
		}
	}
	value.SharedTokenEnv = strings.TrimSpace(value.SharedTokenEnv)
	if value.SharedTokenEnv == "" {
		value.SharedTokenEnv = defaults.SharedTokenEnv
	}
	if !environmentNamePattern.MatchString(value.SharedTokenEnv) {
		return errors.New("capabilities.shared_token_env 不是有效的环境变量名")
	}
	sticker := &value.Sticker
	sticker.DefaultProvider = strings.TrimSpace(sticker.DefaultProvider)
	if sticker.MaxCandidates <= 0 {
		sticker.MaxCandidates = defaults.Sticker.MaxCandidates
	}
	if sticker.MaxCandidates > 20 {
		return errors.New("capabilities.sticker.max_candidates 不能大于 20")
	}
	if sticker.CandidateTTLSeconds <= 0 {
		sticker.CandidateTTLSeconds = defaults.Sticker.CandidateTTLSeconds
	}
	if sticker.CandidateTTLSeconds > 3600 {
		return errors.New("capabilities.sticker.candidate_ttl_seconds 不能大于 3600")
	}
	if sticker.MaxMediaBytes <= 0 {
		sticker.MaxMediaBytes = defaults.Sticker.MaxMediaBytes
	}
	if sticker.MaxMediaBytes > 8<<20 {
		return errors.New("capabilities.sticker.max_media_bytes 不能大于 8 MiB")
	}
	if sticker.MaterializedCacheMaxBytes <= 0 {
		sticker.MaterializedCacheMaxBytes = defaults.Sticker.MaterializedCacheMaxBytes
	}
	if sticker.MaterializedCacheMaxBytes < int64(sticker.MaxMediaBytes) {
		return errors.New("capabilities.sticker.materialized_cache_max_bytes 不能小于 max_media_bytes")
	}
	if err := normalizeStickerProviders(sticker); err != nil {
		return err
	}
	return normalizeVideoCapability(&value.Video, defaults.Video)
}

var (
	environmentNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	providerIDPattern      = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
)

func normalizeStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		key := strings.ToLower(value)
		if key == "" {
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, value)
	}
	return result
}
