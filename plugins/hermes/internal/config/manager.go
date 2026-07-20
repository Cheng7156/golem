package config

import "sync/atomic"

type Snapshot struct {
	Version uint64
	Config
}

type Manager struct {
	next    atomic.Uint64
	current atomic.Pointer[Snapshot]
}

func NewManager(initial Config) (*Manager, error) {
	manager := new(Manager)
	if _, err := manager.Publish(initial); err != nil {
		return nil, err
	}
	return manager, nil
}

func (m *Manager) Publish(value Config) (*Snapshot, error) {
	normalized, err := Normalize(value)
	if err != nil {
		return nil, err
	}
	snapshot := &Snapshot{Version: m.next.Add(1), Config: normalized}
	m.current.Store(snapshot)
	return cloneSnapshot(snapshot), nil
}

func (m *Manager) Current() *Snapshot {
	return cloneSnapshot(m.current.Load())
}

func cloneSnapshot(value *Snapshot) *Snapshot {
	if value == nil {
		return nil
	}
	clone := *value
	clone.BotNames = append([]string(nil), value.BotNames...)
	clone.Capabilities.Sticker.Providers = cloneStickerProviders(value.Capabilities.Sticker.Providers)
	clone.Capabilities.Video.Providers = cloneVideoProviders(value.Capabilities.Video.Providers)
	return &clone
}

func cloneVideoProviders(values []VideoProviderConfig) []VideoProviderConfig {
	result := make([]VideoProviderConfig, len(values))
	for index, provider := range values {
		provider.Categories = append([]string(nil), provider.Categories...)
		provider.Headers = cloneStringMap(provider.Headers)
		provider.MediaHeaders = cloneStringMap(provider.MediaHeaders)
		provider.Query = cloneStringMap(provider.Query)
		provider.Form = cloneStringMap(provider.Form)
		provider.JSONBody = cloneAnyMap(provider.JSONBody)
		provider.AllowedMediaHosts = append([]string(nil), provider.AllowedMediaHosts...)
		provider.Response.SuccessValues = append([]string(nil), provider.Response.SuccessValues...)
		cloneVideoResponseMappings(&provider.Response)
		result[index] = provider
	}
	return result
}

func cloneAnyMap(value map[string]any) map[string]any {
	if value == nil {
		return nil
	}
	result := make(map[string]any, len(value))
	for key, item := range value {
		result[key] = cloneAnyValue(item)
	}
	return result
}

func cloneAnyValue(value any) any {
	switch item := value.(type) {
	case map[string]any:
		return cloneAnyMap(item)
	case []any:
		result := make([]any, len(item))
		for index := range item {
			result[index] = cloneAnyValue(item[index])
		}
		return result
	default:
		return item
	}
}

func cloneVideoResponseMappings(response *VideoResponseConfig) {
	response.URL.Transforms = append([]string(nil), response.URL.Transforms...)
	response.Title.Transforms = append([]string(nil), response.Title.Transforms...)
	response.PageURL.Transforms = append([]string(nil), response.PageURL.Transforms...)
	response.Duration.Transforms = append([]string(nil), response.Duration.Transforms...)
}

func cloneStickerProviders(values []StickerProviderConfig) []StickerProviderConfig {
	result := make([]StickerProviderConfig, len(values))
	for index, provider := range values {
		provider.Headers = cloneStringMap(provider.Headers)
		provider.Query = cloneStringMap(provider.Query)
		provider.Form = cloneStringMap(provider.Form)
		provider.AllowedMediaHosts = append([]string(nil), provider.AllowedMediaHosts...)
		provider.Response.SuccessValues = append([]string(nil), provider.Response.SuccessValues...)
		provider.Response.URL.Transforms = append([]string(nil), provider.Response.URL.Transforms...)
		provider.Response.Description.Transforms = append([]string(nil), provider.Response.Description.Transforms...)
		result[index] = provider
	}
	return result
}

func cloneStringMap(value map[string]string) map[string]string {
	if value == nil {
		return nil
	}
	clone := make(map[string]string, len(value))
	for key, item := range value {
		clone[key] = item
	}
	return clone
}
