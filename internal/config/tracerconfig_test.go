// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2025 Datadog, Inc.

package config

import (
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/DataDog/dd-trace-go/v2/internal/telemetry"
	"github.com/DataDog/dd-trace-go/v2/internal/telemetry/telemetrytest"
)

func TestGetTracerConfig(t *testing.T) {
	t.Run("returns non-nil", func(t *testing.T) {
		ResetConfig()
		defer ResetConfig()

		cfg := GetTracerConfig()
		assert.NotNil(t, cfg)
	})

	t.Run("each call returns independent instance", func(t *testing.T) {
		ResetConfig()
		defer ResetConfig()

		cfg1 := GetTracerConfig()
		cfg2 := GetTracerConfig()
		assert.NotSame(t, cfg1, cfg2)
	})

	t.Run("instances share same BaseConfig", func(t *testing.T) {
		ResetConfig()
		defer ResetConfig()

		cfg1 := GetTracerConfig()
		cfg2 := GetTracerConfig()
		assert.Same(t, cfg1.BaseConfig, cfg2.BaseConfig)
	})

	t.Run("concurrent access is safe", func(t *testing.T) {
		ResetConfig()
		defer ResetConfig()

		const numGoroutines = 50
		var wg sync.WaitGroup
		wg.Add(numGoroutines)

		var uniqueInstances sync.Map
		var configCount atomic.Int32

		for range numGoroutines {
			go func() {
				defer wg.Done()
				cfg := GetTracerConfig()
				require.NotNil(t, cfg)
				if _, loaded := uniqueInstances.LoadOrStore(cfg, true); !loaded {
					configCount.Add(1)
				}
			}()
		}
		wg.Wait()

		assert.Greater(t, configCount.Load(), int32(1), "Each GetTracerConfig() call should return a distinct instance")
	})
}

// TestTracerSettersReportTelemetry verifies all Set* methods on TracerConfig
// (shadow overrides + promoted BaseConfig setters) report telemetry.
func TestTracerSettersReportTelemetry(t *testing.T) {
	configType := reflect.TypeFor[*TracerConfig]()

	for i := 0; i < configType.NumMethod(); i++ {
		method := configType.Method(i)
		methodName := method.Name

		if len(methodName) < 3 || methodName[:3] != "Set" {
			continue
		}
		if reason, excluded := settersWithoutTelemetry[methodName]; excluded {
			t.Logf("Skipping %s: %s", methodName, reason)
			continue
		}

		t.Run(methodName, func(t *testing.T) {
			ResetConfig()
			defer ResetConfig()

			telemetryClient := new(telemetrytest.MockClient)
			telemetryClient.On("RegisterAppConfigs", mock.Anything).Return().Maybe()
			defer telemetry.MockClient(telemetryClient)()

			cfg := GetTracerConfig()
			testOrigin := telemetry.OriginCode

			if callFunc, isSpecial := specialCaseSetters[methodName]; isSpecial {
				callFunc(cfg, testOrigin)
			} else {
				callSetter(t, cfg, method, testOrigin)
			}

			foundTelemetry := false
			for _, call := range telemetryClient.Calls {
				if call.Method == "RegisterAppConfigs" {
					if len(call.Arguments) > 0 {
						if configs, ok := call.Arguments[0].([]telemetry.Configuration); ok && len(configs) > 0 {
							if configs[0].Origin == testOrigin {
								foundTelemetry = true
								break
							}
						}
					}
				}
			}

			assert.True(t, foundTelemetry,
				"%s: no telemetry with origin=%v. Fix: call configtelemetry.Report() OR add to settersWithoutTelemetry/specialCaseSetters",
				methodName, testOrigin)
		})
	}
}

func TestSetServiceMappingReportsFullList(t *testing.T) {
	ResetConfig()
	defer ResetConfig()

	rec := new(telemetrytest.RecordClient)
	defer telemetry.MockClient(rec)()

	cfg := GetTracerConfig()
	require.NotNil(t, cfg)

	cfg.SetServiceMapping("b", "2", telemetry.OriginCode)
	cfg.SetServiceMapping("a", "1", telemetry.OriginCode)
	cfg.SetServiceMapping("a", "3", telemetry.OriginCode) // update existing

	var (
		found bool
		got   telemetry.Configuration
	)
	for i := len(rec.Configuration) - 1; i >= 0; i-- {
		c := rec.Configuration[i]
		if c.Name == "DD_SERVICE_MAPPING" && c.Origin == telemetry.OriginCode {
			found = true
			got = c
			break
		}
	}
	require.True(t, found, "expected telemetry to include DD_SERVICE_MAPPING with OriginCode")
	require.IsType(t, "", got.Value)
	parts := strings.Split(got.Value.(string), ",")
	sort.Strings(parts)
	assert.Equal(t, []string{"a:3", "b:2"}, parts)
}

func TestOTLPExportMode(t *testing.T) {
	t.Run("disabled by default", func(t *testing.T) {
		ResetConfig()
		defer ResetConfig()

		cfg := GetTracerConfig()
		require.NotNil(t, cfg)

		assert.False(t, cfg.OTLPExportMode())
	})

	t.Run("enabled by OTEL_TRACES_EXPORTER=otlp", func(t *testing.T) {
		ResetConfig()
		defer ResetConfig()

		t.Setenv("OTEL_TRACES_EXPORTER", "otlp")

		cfg := GetTracerConfig()
		require.NotNil(t, cfg)

		assert.True(t, cfg.OTLPExportMode())
	})

	t.Run("not enabled by unsupported exporter value", func(t *testing.T) {
		ResetConfig()
		defer ResetConfig()

		t.Setenv("OTEL_TRACES_EXPORTER", "jaeger")

		cfg := GetTracerConfig()
		require.NotNil(t, cfg)

		assert.False(t, cfg.OTLPExportMode())
	})

	t.Run("not enabled by OTEL_TRACES_EXPORTER=none", func(t *testing.T) {
		ResetConfig()
		defer ResetConfig()

		t.Setenv("OTEL_TRACES_EXPORTER", "none")

		cfg := GetTracerConfig()
		require.NotNil(t, cfg)

		assert.False(t, cfg.OTLPExportMode())
	})

	t.Run("DD_TRACE_AGENT_PROTOCOL_VERSION overrides OTEL_TRACES_EXPORTER", func(t *testing.T) {
		ResetConfig()
		defer ResetConfig()

		t.Setenv("OTEL_TRACES_EXPORTER", "otlp")
		t.Setenv("DD_TRACE_AGENT_PROTOCOL_VERSION", "1.0")

		cfg := GetTracerConfig()
		require.NotNil(t, cfg)

		assert.False(t, cfg.OTLPExportMode(), "otlpExportMode should be false when DD_TRACE_AGENT_PROTOCOL_VERSION is explicitly set")
		assert.Equal(t, TraceProtocolV1, cfg.TraceProtocol())
	})

	t.Run("DD_TRACE_AGENT_PROTOCOL_VERSION=0.4 still overrides OTEL_TRACES_EXPORTER", func(t *testing.T) {
		ResetConfig()
		defer ResetConfig()

		t.Setenv("OTEL_TRACES_EXPORTER", "otlp")
		t.Setenv("DD_TRACE_AGENT_PROTOCOL_VERSION", "0.4")

		cfg := GetTracerConfig()
		require.NotNil(t, cfg)

		assert.False(t, cfg.OTLPExportMode(), "otlpExportMode should be false when DD_TRACE_AGENT_PROTOCOL_VERSION is explicitly set, even to the default value")
		assert.Equal(t, TraceProtocolV04, cfg.TraceProtocol())
	})

	t.Run("SetOTLPExportMode toggles mode", func(t *testing.T) {
		ResetConfig()
		defer ResetConfig()

		cfg := GetTracerConfig()
		require.NotNil(t, cfg)

		assert.False(t, cfg.OTLPExportMode())

		cfg.SetOTLPExportMode(true, telemetry.OriginCode)
		assert.True(t, cfg.OTLPExportMode())

		cfg.SetOTLPExportMode(false, telemetry.OriginCode)
		assert.False(t, cfg.OTLPExportMode())
	})
}
