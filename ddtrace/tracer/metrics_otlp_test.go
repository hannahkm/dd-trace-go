// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2025 Datadog, Inc.

package tracer

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsOTLPMetricsEnabled(t *testing.T) {
	tests := []struct {
		envVal   string
		expected bool
	}{
		{"true", true},
		{"True", true},
		{"TRUE", true},
		{"1", true},
		{"false", false},
		{"False", false},
		{"0", false},
		{"", false},
		{"invalid", false},
	}
	for _, tt := range tests {
		t.Run(tt.envVal, func(t *testing.T) {
			if tt.envVal != "" {
				t.Setenv("DD_METRICS_OTEL_ENABLED", tt.envVal)
			}
			assert.Equal(t, tt.expected, isOTLPMetricsEnabled())
		})
	}
}

func TestTracerOTLPRuntimeMetricsToggle(t *testing.T) {
	// When DD_METRICS_OTEL_ENABLED is false, OTLP runtime metrics should not start
	t.Setenv("DD_METRICS_OTEL_ENABLED", "false")
	assert.False(t, isOTLPMetricsEnabled())

	// When DD_METRICS_OTEL_ENABLED is true, it should be detected
	t.Setenv("DD_METRICS_OTEL_ENABLED", "true")
	assert.True(t, isOTLPMetricsEnabled())
}
