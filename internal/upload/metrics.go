package upload

import "time"

// ThroughputMiBPerSecond returns bytes per second converted to MiB/s.
func ThroughputMiBPerSecond(bytes int64, elapsed time.Duration) float64 {
	if bytes <= 0 || elapsed <= 0 {
		return 0
	}
	return float64(bytes) / elapsed.Seconds() / 1024 / 1024
}
