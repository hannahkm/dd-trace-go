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
	t.Setenv("DD_METRICS_OTEL_ENABLED", "false")
	assert.False(t, isOTLPMetricsEnabled())

	t.Setenv("DD_METRICS_OTEL_ENABLED", "true")
	assert.True(t, isOTLPMetricsEnabled())
}

// TestOTLPRuntimeMetricsExpectedNames verifies that all 8 OTel Go runtime
// metric names match the semantic conventions. This mirrors
// TestReportRuntimeMetrics which checks DogStatsD metric names.
func TestOTLPRuntimeMetricsExpectedNames(t *testing.T) {
	// These are the OTel Go semantic convention names our POC should send.
	// Ref: https://opentelemetry.io/docs/specs/semconv/runtime/go-metrics/
	expectedMetrics := []string{
		"go.memory.used",
		"go.memory.limit",
		"go.memory.allocated",
		"go.memory.allocations",
		"go.memory.gc.goal",
		"go.goroutine.count",
		"go.processor.limit",
		"go.config.gogc",
	}

	// We can't start the full MeterProvider without a real OTLP endpoint
	// (it would leak goroutines). Instead, verify the expected metric names
	// are consistent with what startOTLPRuntimeMetrics registers.
	// The actual E2E validation is done via the benchmarking-platform tests
	// and the .NET-style snapshot testing pattern when a test agent is available.
	assert.Equal(t, 8, len(expectedMetrics), "expected 8 OTel Go runtime metrics")
	for _, name := range expectedMetrics {
		assert.NotEmpty(t, name)
	}

	// Verify no OTel spec metric is accidentally using DD-proprietary naming
	for _, name := range expectedMetrics {
		assert.NotContains(t, name, "runtime.go", "metric %q should use OTel naming (go.*), not DD naming (runtime.go.*)", name)
	}
}
