package upload

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestThroughputMiBPerSecond(t *testing.T) {
	got := ThroughputMiBPerSecond(100*1024*1024, 10*time.Second)
	assert.Equal(t, 10.0, got)
}

func TestThroughputMiBPerSecondZeroInputs(t *testing.T) {
	assert.Equal(t, 0.0, ThroughputMiBPerSecond(0, 10*time.Second))
	assert.Equal(t, 0.0, ThroughputMiBPerSecond(1024, 0))
}
