package output

import (
	"math/rand/v2"
	"time"

	"golem_plugin_hermes/internal/config"
)

func retryDelay(config config.OutputConfig, attempt int) time.Duration {
	minimum := time.Duration(config.RetryMinSeconds) * time.Second
	maximum := time.Duration(config.RetryMaxSeconds) * time.Second
	delay := minimum * time.Duration(1<<min(max(0, attempt-1), 20))
	if delay > maximum {
		delay = maximum
	}
	jitter := time.Duration(0)
	if config.SendJitterMilliseconds > 0 {
		jitter = time.Duration(rand.IntN(config.SendJitterMilliseconds+1)) * time.Millisecond
	}
	return delay + jitter
}
