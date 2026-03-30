// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2025 Datadog, Inc.

package config

import (
	"net/url"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/DataDog/dd-trace-go/v2/internal/telemetry"
	"github.com/DataDog/dd-trace-go/v2/internal/telemetry/telemetrytest"
)

func TestGetSharedConfig(t *testing.T) {
	t.Run("returns non-nil", func(t *testing.T) {
		resetGlobalState()
		defer resetGlobalState()

		cfg := GetSharedConfig()
		assert.NotNil(t, cfg)
	})

	t.Run("singleton - returns same instance", func(t *testing.T) {
		resetGlobalState()
		defer resetGlobalState()

		cfg1 := GetSharedConfig()
		cfg2 := GetSharedConfig()
		assert.Same(t, cfg1, cfg2)
	})

	t.Run("SetUseFreshConfig resets singleton", func(t *testing.T) {
		resetGlobalState()
		defer resetGlobalState()

		cfg1 := GetSharedConfig()
		require.NotNil(t, cfg1)

		SetUseFreshConfig(true)

		cfg2 := GetSharedConfig()
		require.NotNil(t, cfg2)
		assert.NotSame(t, cfg1, cfg2, "SetUseFreshConfig should reset the SharedConfig singleton")
	})

	t.Run("concurrent access is safe", func(t *testing.T) {
		resetGlobalState()
		defer resetGlobalState()

		const numGoroutines = 100
		var wg sync.WaitGroup
		wg.Add(numGoroutines)

		configs := make([]*SharedConfig, numGoroutines)
		for i := range numGoroutines {
			go func(j int) {
				defer wg.Done()
				configs[j] = GetSharedConfig()
			}(i)
		}
		wg.Wait()

		for i, cfg := range configs {
			assert.NotNil(t, cfg, "Config at index %d should not be nil", i)
		}
		for i, cfg := range configs[1:] {
			assert.Same(t, configs[0], cfg, "Config at index %d should be the same instance", i+1)
		}
	})

	t.Run("loads values from env", func(t *testing.T) {
		resetGlobalState()
		defer resetGlobalState()

		t.Setenv("DD_TRACE_DEBUG", "true")

		cfg := GetSharedConfig()
		require.NotNil(t, cfg)
		assert.True(t, cfg.Debug())
	})

	t.Run("setters update config thread-safely", func(t *testing.T) {
		resetGlobalState()
		defer resetGlobalState()

		cfg := GetSharedConfig()
		require.NotNil(t, cfg)

		initialDebug := cfg.Debug()
		cfg.SetDebug(!initialDebug, "test")
		assert.Equal(t, !initialDebug, cfg.Debug())

		var wg sync.WaitGroup
		const numOperations = 100

		wg.Add(numOperations * 2)
		for range numOperations {
			go func() {
				defer wg.Done()
				_ = cfg.Debug()
			}()
		}
		for i := range numOperations {
			go func(val bool) {
				defer wg.Done()
				cfg.SetDebug(val, "test")
			}(i%2 == 0)
		}
		wg.Wait()

		assert.IsType(t, true, cfg.Debug())
	})
}

func TestGetProfilerConfig(t *testing.T) {
	t.Run("returns non-nil", func(t *testing.T) {
		resetGlobalState()
		defer resetGlobalState()

		cfg := GetProfilerConfig()
		assert.NotNil(t, cfg)
	})

	t.Run("shares same SharedConfig as tracer", func(t *testing.T) {
		resetGlobalState()
		defer resetGlobalState()

		tc := GetTracerConfig()
		pc := GetProfilerConfig()
		assert.Same(t, tc.SharedConfig, pc.SharedConfig)
	})
}

// TestSharedSettersReportTelemetry verifies all Set* methods on SharedConfig report telemetry.
func TestSharedSettersReportTelemetry(t *testing.T) {
	configType := reflect.TypeFor[*SharedConfig]()

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
			resetGlobalState()
			defer resetGlobalState()

			telemetryClient := new(telemetrytest.MockClient)
			telemetryClient.On("RegisterAppConfigs", mock.Anything).Return().Maybe()
			defer telemetry.MockClient(telemetryClient)()

			cfg := GetSharedConfig()
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
				"%s: no telemetry with origin=%v. Fix: call configtelemetry.Report() OR add to settersWithoutTelemetry",
				methodName, testOrigin)
		})
	}
}

// ---------------------------------------------------------------------------
// Shared test helpers (used by config_test.go and tracerconfig_test.go)
// ---------------------------------------------------------------------------

// settersWithoutTelemetry lists Set methods that don't report telemetry.
// Add your setter here with a reason if telemetry reporting is not needed.
var settersWithoutTelemetry = map[string]string{
	"SetLogToStdout":      "not user-configurable",
	"SetIsLambdaFunction": "not user-configurable",
}

// specialCaseSetters handles setters with non-standard signatures.
// Add here if signature is not: SetX(value T, origin telemetry.Origin)
var specialCaseSetters = map[string]func(any, telemetry.Origin){
	"SetServiceMapping": func(cfg any, origin telemetry.Origin) {
		reflect.ValueOf(cfg).MethodByName("SetServiceMapping").Call([]reflect.Value{
			reflect.ValueOf("from-service"),
			reflect.ValueOf("to-service"),
			reflect.ValueOf(origin),
		})
	},
}

// callSetter attempts to call a setter method with appropriate test values.
// cfg must be a pointer to the config struct that owns the method.
func callSetter(t *testing.T, cfg any, method reflect.Method, origin telemetry.Origin) {
	methodType := method.Type

	if methodType.NumIn() < 3 {
		t.Fatalf("%s: expected ≥3 params (receiver, value, origin), got %d. Add to specialCaseSetters if non-standard.",
			method.Name, methodType.NumIn())
	}

	originType := reflect.TypeFor[telemetry.Origin]()
	lastParamType := methodType.In(methodType.NumIn() - 1)
	if lastParamType != originType {
		t.Fatalf("%s: last param should be telemetry.Origin, got %v. Add to specialCaseSetters if non-standard.",
			method.Name, lastParamType)
	}

	args := []reflect.Value{reflect.ValueOf(cfg)}
	for i := 1; i < methodType.NumIn()-1; i++ {
		paramType := methodType.In(i)
		testValue := getTestValueForType(paramType)
		args = append(args, reflect.ValueOf(testValue))
	}
	args = append(args, reflect.ValueOf(origin))
	method.Func.Call(args)
}

// getTestValueForType generates appropriate test values based on parameter type.
// Add support for new types here as setters with new parameter types are added.
func getTestValueForType(t reflect.Type) any {
	if t == reflect.TypeFor[time.Duration]() {
		return 10 * time.Second
	}
	if t == reflect.TypeFor[*url.URL]() {
		return &url.URL{Scheme: "http", Host: "test-agent:8126"}
	}

	switch t.Kind() {
	case reflect.Bool:
		return true
	case reflect.String:
		return "test-value"
	case reflect.Int:
		return 42
	case reflect.Float64:
		return 0.75
	case reflect.Slice:
		if t.Elem().Kind() == reflect.String {
			return []string{"feature1", "feature2"}
		}
	}

	panic("getTestValueForType: unsupported parameter type: " + t.String() +
		". Add support for this type in getTestValueForType() or add your setter to specialCaseSetters.")
}

// resetGlobalState resets all global singleton state for testing.
func resetGlobalState() {
	globalMu = sync.Mutex{}
	globalInstance = nil
	globalProvider = nil
}

// ---------------------------------------------------------------------------
// SharedConfig-specific tests
// ---------------------------------------------------------------------------

func TestSetFeatureFlagsReportsFullList(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()

	rec := new(telemetrytest.RecordClient)
	defer telemetry.MockClient(rec)()

	cfg := GetSharedConfig()
	require.NotNil(t, cfg)

	cfg.SetFeatureFlags([]string{"b", "a"}, telemetry.OriginCode)
	cfg.SetFeatureFlags([]string{"c"}, telemetry.OriginCode)

	var (
		found bool
		got   telemetry.Configuration
	)
	for i := len(rec.Configuration) - 1; i >= 0; i-- {
		c := rec.Configuration[i]
		if c.Name == "DD_TRACE_FEATURES" && c.Origin == telemetry.OriginCode {
			found = true
			got = c
			break
		}
	}
	require.True(t, found, "expected telemetry to include DD_TRACE_FEATURES with OriginCode")
	require.IsType(t, "", got.Value)
	parts := strings.Split(got.Value.(string), ",")
	sort.Strings(parts)
	assert.Equal(t, []string{"a", "b", "c"}, parts)
}

func TestOTLPTraceURLResolution(t *testing.T) {
	t.Run("default OTLP port from agent host", func(t *testing.T) {
		resetGlobalState()
		defer resetGlobalState()

		cfg := GetSharedConfig()
		require.NotNil(t, cfg)

		assert.Contains(t, cfg.OTLPTraceURL(), ":4318/v1/traces")
	})

	t.Run("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT overrides", func(t *testing.T) {
		resetGlobalState()
		defer resetGlobalState()

		t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "http://collector:4318/v1/traces")

		cfg := GetSharedConfig()
		require.NotNil(t, cfg)

		assert.Equal(t, "http://collector:4318/v1/traces", cfg.OTLPTraceURL())
	})

	t.Run("uses agent host when no OTLP endpoint configured", func(t *testing.T) {
		resetGlobalState()
		defer resetGlobalState()

		t.Setenv("DD_AGENT_HOST", "custom-agent")

		cfg := GetSharedConfig()
		require.NotNil(t, cfg)

		assert.Equal(t, "http://custom-agent:4318/v1/traces", cfg.OTLPTraceURL())
	})
}

func TestOTLPHeaders(t *testing.T) {
	t.Run("always populated with at least Content-Type", func(t *testing.T) {
		resetGlobalState()
		defer resetGlobalState()

		cfg := GetSharedConfig()
		require.NotNil(t, cfg)

		headers := cfg.OTLPHeaders()
		require.NotNil(t, headers)
		assert.Equal(t, OTLPContentTypeHeader, headers["Content-Type"])
		assert.Len(t, headers, 1)
	})

	t.Run("OTEL_EXPORTER_OTLP_TRACES_HEADERS parsed into map", func(t *testing.T) {
		resetGlobalState()
		defer resetGlobalState()

		t.Setenv("OTEL_EXPORTER_OTLP_TRACES_HEADERS", "api-key=secret,x-custom=value")

		cfg := GetSharedConfig()
		require.NotNil(t, cfg)

		headers := cfg.OTLPHeaders()
		assert.Equal(t, "secret", headers["api-key"])
		assert.Equal(t, "value", headers["x-custom"])
		assert.Equal(t, OTLPContentTypeHeader, headers["Content-Type"])
	})
}

func TestHostnameConfiguration(t *testing.T) {
	t.Run("default behavior - hostname empty when not configured", func(t *testing.T) {
		resetGlobalState()
		defer resetGlobalState()

		cfg := GetSharedConfig()
		require.NotNil(t, cfg)

		assert.Empty(t, cfg.Hostname(), "Hostname should be empty by default")
		assert.False(t, cfg.ReportHostname(), "ReportHostname should be false by default")
	})

	t.Run("DD_TRACE_REPORT_HOSTNAME=true enables hostname lookup", func(t *testing.T) {
		resetGlobalState()
		defer resetGlobalState()

		t.Setenv("DD_TRACE_REPORT_HOSTNAME", "true")

		cfg := GetSharedConfig()
		require.NotNil(t, cfg)

		assert.NotEmpty(t, cfg.Hostname(), "Hostname should be set when DD_TRACE_REPORT_HOSTNAME=true")
		assert.True(t, cfg.ReportHostname(), "ReportHostname should be true when DD_TRACE_REPORT_HOSTNAME=true")
		assert.NoError(t, cfg.HostnameLookupError(), "HostnameLookupError should be nil on successful lookup")
	})

	t.Run("DD_TRACE_REPORT_HOSTNAME=false keeps hostname empty", func(t *testing.T) {
		resetGlobalState()
		defer resetGlobalState()

		t.Setenv("DD_TRACE_REPORT_HOSTNAME", "false")

		cfg := GetSharedConfig()
		require.NotNil(t, cfg)

		assert.Empty(t, cfg.Hostname(), "Hostname should be empty when DD_TRACE_REPORT_HOSTNAME=false")
		assert.False(t, cfg.ReportHostname(), "ReportHostname should be false when DD_TRACE_REPORT_HOSTNAME=false")
	})

	t.Run("DD_TRACE_SOURCE_HOSTNAME sets explicit hostname", func(t *testing.T) {
		resetGlobalState()
		defer resetGlobalState()

		t.Setenv("DD_TRACE_SOURCE_HOSTNAME", "custom-hostname")

		cfg := GetSharedConfig()
		require.NotNil(t, cfg)

		assert.Equal(t, "custom-hostname", cfg.Hostname(), "Hostname should match DD_TRACE_SOURCE_HOSTNAME")
		assert.True(t, cfg.ReportHostname(), "ReportHostname should be true when DD_TRACE_SOURCE_HOSTNAME is set")
	})

	t.Run("DD_TRACE_SOURCE_HOSTNAME takes precedence over DD_TRACE_REPORT_HOSTNAME", func(t *testing.T) {
		resetGlobalState()
		defer resetGlobalState()

		t.Setenv("DD_TRACE_REPORT_HOSTNAME", "true")
		t.Setenv("DD_TRACE_SOURCE_HOSTNAME", "override-hostname")

		cfg := GetSharedConfig()
		require.NotNil(t, cfg)

		assert.Equal(t, "override-hostname", cfg.Hostname(), "DD_TRACE_SOURCE_HOSTNAME should take precedence")
		assert.True(t, cfg.ReportHostname(), "ReportHostname should be true")
	})

	t.Run("empty DD_TRACE_SOURCE_HOSTNAME is used when explicitly set", func(t *testing.T) {
		resetGlobalState()
		defer resetGlobalState()

		t.Setenv("DD_TRACE_REPORT_HOSTNAME", "true")
		t.Setenv("DD_TRACE_SOURCE_HOSTNAME", "")

		cfg := GetSharedConfig()
		require.NotNil(t, cfg)

		assert.Empty(t, cfg.Hostname(), "Empty DD_TRACE_SOURCE_HOSTNAME should override hostname lookup")
		assert.True(t, cfg.ReportHostname(), "ReportHostname should be true when DD_TRACE_SOURCE_HOSTNAME is explicitly set")
	})
}
