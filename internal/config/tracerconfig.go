// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2025 Datadog, Inc.

package config

import (
	"fmt"
	"maps"
	"strings"
	"sync"
	"time"

	"github.com/DataDog/dd-trace-go/v2/internal/civisibility/constants"
	configtelemetry "github.com/DataDog/dd-trace-go/v2/internal/config/configtelemetry"
	"github.com/DataDog/dd-trace-go/v2/internal/telemetry"
	"github.com/DataDog/dd-trace-go/v2/internal/traceprof"
)

// TracerConfig holds tracer-specific configuration. It embeds a pointer to
// the shared BaseConfig so shared field accessors are promoted.
//
// Any field that the tracer's programmatic API (With* options) can set
// is represented here as a shadow field. A nil shadow means "use the
// BaseConfig value"; a non-nil shadow means the programmatic API has
// provided a local override. This keeps env-var / RC updates flowing
// through BaseConfig to all products, while programmatic overrides
// stay local to the tracer.
type TracerConfig struct {
	*BaseConfig

	tmu sync.RWMutex // protects TracerConfig fields only

	// Shadow fields — nil means "use BaseConfig value".
	serviceName             *string
	env                     *string
	version                 *string
	serviceMappings         map[string]string // nil = use BaseConfig
	runtimeMetrics          *bool
	runtimeMetricsV2        *bool
	profilerHotspots        *bool
	profilerEndpoints       *bool
	debugAbandonedSpans     *bool
	spanTimeout             *time.Duration
	partialFlushEnabled     *bool
	partialFlushMinSpans    *int
	statsComputationEnabled *bool
	globalSampleRate        *float64
	traceRateLimitPerSecond *float64
	debugStack              *bool
	retryInterval           *time.Duration
	traceProtocol           *float64
	otlpExportMode          *bool
	ciVisibilityEnabled     *bool
}

func loadTracerConfig(g *BaseConfig) *TracerConfig {
	return &TracerConfig{BaseConfig: g}
}

// ---------------------------------------------------------------------------
// Shadow getters & setters
// ---------------------------------------------------------------------------

func (t *TracerConfig) ServiceName() string {
	t.tmu.RLock()
	if t.serviceName != nil {
		v := *t.serviceName
		t.tmu.RUnlock()
		return v
	}
	t.tmu.RUnlock()
	return t.BaseConfig.ServiceName()
}

func (t *TracerConfig) SetServiceName(name string, origin telemetry.Origin) {
	t.tmu.Lock()
	t.serviceName = &name
	t.tmu.Unlock()
	configtelemetry.Report("DD_SERVICE", name, origin)
}

func (t *TracerConfig) Env() string {
	t.tmu.RLock()
	if t.env != nil {
		v := *t.env
		t.tmu.RUnlock()
		return v
	}
	t.tmu.RUnlock()
	return t.BaseConfig.Env()
}

func (t *TracerConfig) SetEnv(env string, origin telemetry.Origin) {
	t.tmu.Lock()
	t.env = &env
	t.tmu.Unlock()
	configtelemetry.Report("DD_ENV", env, origin)
}

func (t *TracerConfig) Version() string {
	t.tmu.RLock()
	if t.version != nil {
		v := *t.version
		t.tmu.RUnlock()
		return v
	}
	t.tmu.RUnlock()
	return t.BaseConfig.Version()
}

func (t *TracerConfig) SetVersion(version string, origin telemetry.Origin) {
	t.tmu.Lock()
	t.version = &version
	t.tmu.Unlock()
	configtelemetry.Report("DD_VERSION", version, origin)
}

// ServiceMappings returns a copy of the tracer's local mappings if any
// programmatic override has been applied, otherwise the shared mappings.
func (t *TracerConfig) ServiceMappings() map[string]string {
	t.tmu.RLock()
	defer t.tmu.RUnlock()
	if t.serviceMappings != nil {
		result := make(map[string]string, len(t.serviceMappings))
		maps.Copy(result, t.serviceMappings)
		return result
	}
	return t.BaseConfig.ServiceMappings()
}

// ServiceMapping performs a single mapping lookup.
func (t *TracerConfig) ServiceMapping(from string) (to string, ok bool) {
	t.tmu.RLock()
	if t.serviceMappings != nil {
		to, ok = t.serviceMappings[from]
		t.tmu.RUnlock()
		return to, ok
	}
	t.tmu.RUnlock()
	return t.BaseConfig.ServiceMapping(from)
}

func (t *TracerConfig) SetServiceMapping(from, to string, origin telemetry.Origin) {
	t.tmu.Lock()
	if t.serviceMappings == nil {
		shared := t.BaseConfig.ServiceMappings()
		if shared != nil {
			t.serviceMappings = shared
		} else {
			t.serviceMappings = make(map[string]string)
		}
	}
	t.serviceMappings[from] = to
	all := make([]string, 0, len(t.serviceMappings))
	for k, v := range t.serviceMappings {
		all = append(all, fmt.Sprintf("%s:%s", k, v))
	}
	t.tmu.Unlock()
	configtelemetry.Report("DD_SERVICE_MAPPING", strings.Join(all, ","), origin)
}

func (t *TracerConfig) RuntimeMetricsEnabled() bool {
	t.tmu.RLock()
	if t.runtimeMetrics != nil {
		v := *t.runtimeMetrics
		t.tmu.RUnlock()
		return v
	}
	t.tmu.RUnlock()
	return t.BaseConfig.RuntimeMetricsEnabled()
}

func (t *TracerConfig) SetRuntimeMetricsEnabled(enabled bool, origin telemetry.Origin) {
	t.tmu.Lock()
	t.runtimeMetrics = &enabled
	t.tmu.Unlock()
	configtelemetry.Report("DD_RUNTIME_METRICS_ENABLED", enabled, origin)
}

func (t *TracerConfig) RuntimeMetricsV2Enabled() bool {
	t.tmu.RLock()
	if t.runtimeMetricsV2 != nil {
		v := *t.runtimeMetricsV2
		t.tmu.RUnlock()
		return v
	}
	t.tmu.RUnlock()
	return t.BaseConfig.RuntimeMetricsV2Enabled()
}

func (t *TracerConfig) SetRuntimeMetricsV2Enabled(enabled bool, origin telemetry.Origin) {
	t.tmu.Lock()
	t.runtimeMetricsV2 = &enabled
	t.tmu.Unlock()
	configtelemetry.Report("DD_RUNTIME_METRICS_V2_ENABLED", enabled, origin)
}

func (t *TracerConfig) ProfilerHotspotsEnabled() bool {
	t.tmu.RLock()
	if t.profilerHotspots != nil {
		v := *t.profilerHotspots
		t.tmu.RUnlock()
		return v
	}
	t.tmu.RUnlock()
	return t.BaseConfig.ProfilerHotspotsEnabled()
}

func (t *TracerConfig) SetProfilerHotspotsEnabled(enabled bool, origin telemetry.Origin) {
	t.tmu.Lock()
	t.profilerHotspots = &enabled
	t.tmu.Unlock()
	configtelemetry.Report(traceprof.CodeHotspotsEnvVar, enabled, origin)
}

func (t *TracerConfig) ProfilerEndpoints() bool {
	t.tmu.RLock()
	if t.profilerEndpoints != nil {
		v := *t.profilerEndpoints
		t.tmu.RUnlock()
		return v
	}
	t.tmu.RUnlock()
	return t.BaseConfig.ProfilerEndpoints()
}

func (t *TracerConfig) SetProfilerEndpoints(enabled bool, origin telemetry.Origin) {
	t.tmu.Lock()
	t.profilerEndpoints = &enabled
	t.tmu.Unlock()
	configtelemetry.Report("DD_PROFILING_ENDPOINT_COLLECTION_ENABLED", enabled, origin)
}

func (t *TracerConfig) DebugAbandonedSpans() bool {
	t.tmu.RLock()
	if t.debugAbandonedSpans != nil {
		v := *t.debugAbandonedSpans
		t.tmu.RUnlock()
		return v
	}
	t.tmu.RUnlock()
	return t.BaseConfig.DebugAbandonedSpans()
}

func (t *TracerConfig) SetDebugAbandonedSpans(enabled bool, origin telemetry.Origin) {
	t.tmu.Lock()
	t.debugAbandonedSpans = &enabled
	t.tmu.Unlock()
	configtelemetry.Report("DD_TRACE_DEBUG_ABANDONED_SPANS", enabled, origin)
}

func (t *TracerConfig) SpanTimeout() time.Duration {
	t.tmu.RLock()
	if t.spanTimeout != nil {
		v := *t.spanTimeout
		t.tmu.RUnlock()
		return v
	}
	t.tmu.RUnlock()
	return t.BaseConfig.SpanTimeout()
}

func (t *TracerConfig) SetSpanTimeout(timeout time.Duration, origin telemetry.Origin) {
	t.tmu.Lock()
	t.spanTimeout = &timeout
	t.tmu.Unlock()
	configtelemetry.Report("DD_TRACE_ABANDONED_SPAN_TIMEOUT", timeout, origin)
}

// PartialFlushEnabled returns the partial flushing configuration.
// Each field resolves independently: local override if set, otherwise shared value.
func (t *TracerConfig) PartialFlushEnabled() (enabled bool, minSpans int) {
	t.tmu.RLock()
	localEnabled := t.partialFlushEnabled
	localMinSpans := t.partialFlushMinSpans
	t.tmu.RUnlock()

	sharedEnabled, sharedMinSpans := t.BaseConfig.PartialFlushEnabled()

	if localEnabled != nil {
		enabled = *localEnabled
	} else {
		enabled = sharedEnabled
	}
	if localMinSpans != nil {
		minSpans = *localMinSpans
	} else {
		minSpans = sharedMinSpans
	}
	return enabled, minSpans
}

func (t *TracerConfig) SetPartialFlushEnabled(enabled bool, origin telemetry.Origin) {
	t.tmu.Lock()
	t.partialFlushEnabled = &enabled
	t.tmu.Unlock()
	configtelemetry.Report("DD_TRACE_PARTIAL_FLUSH_ENABLED", enabled, origin)
}

func (t *TracerConfig) SetPartialFlushMinSpans(minSpans int, origin telemetry.Origin) {
	t.tmu.Lock()
	t.partialFlushMinSpans = &minSpans
	t.tmu.Unlock()
	configtelemetry.Report("DD_TRACE_PARTIAL_FLUSH_MIN_SPANS", minSpans, origin)
}

func (t *TracerConfig) StatsComputationEnabled() bool {
	t.tmu.RLock()
	if t.statsComputationEnabled != nil {
		v := *t.statsComputationEnabled
		t.tmu.RUnlock()
		return v
	}
	t.tmu.RUnlock()
	return t.BaseConfig.StatsComputationEnabled()
}

func (t *TracerConfig) SetStatsComputationEnabled(enabled bool, origin telemetry.Origin) {
	t.tmu.Lock()
	t.statsComputationEnabled = &enabled
	t.tmu.Unlock()
	configtelemetry.Report("DD_TRACE_STATS_COMPUTATION_ENABLED", enabled, origin)
}

func (t *TracerConfig) GlobalSampleRate() float64 {
	t.tmu.RLock()
	if t.globalSampleRate != nil {
		v := *t.globalSampleRate
		t.tmu.RUnlock()
		return v
	}
	t.tmu.RUnlock()
	return t.BaseConfig.GlobalSampleRate()
}

func (t *TracerConfig) SetGlobalSampleRate(rate float64, origin telemetry.Origin) {
	t.tmu.Lock()
	t.globalSampleRate = &rate
	t.tmu.Unlock()
	configtelemetry.Report("DD_TRACE_SAMPLE_RATE", rate, origin)
}

func (t *TracerConfig) TraceRateLimitPerSecond() float64 {
	t.tmu.RLock()
	if t.traceRateLimitPerSecond != nil {
		v := *t.traceRateLimitPerSecond
		t.tmu.RUnlock()
		return v
	}
	t.tmu.RUnlock()
	return t.BaseConfig.TraceRateLimitPerSecond()
}

func (t *TracerConfig) SetTraceRateLimitPerSecond(rate float64, origin telemetry.Origin) {
	t.tmu.Lock()
	t.traceRateLimitPerSecond = &rate
	t.tmu.Unlock()
	configtelemetry.Report("DD_TRACE_RATE_LIMIT", rate, origin)
}

func (t *TracerConfig) DebugStack() bool {
	t.tmu.RLock()
	if t.debugStack != nil {
		v := *t.debugStack
		t.tmu.RUnlock()
		return v
	}
	t.tmu.RUnlock()
	return t.BaseConfig.DebugStack()
}

func (t *TracerConfig) SetDebugStack(enabled bool, origin telemetry.Origin) {
	t.tmu.Lock()
	t.debugStack = &enabled
	t.tmu.Unlock()
	configtelemetry.Report("DD_TRACE_DEBUG_STACK", enabled, origin)
}

func (t *TracerConfig) RetryInterval() time.Duration {
	t.tmu.RLock()
	if t.retryInterval != nil {
		v := *t.retryInterval
		t.tmu.RUnlock()
		return v
	}
	t.tmu.RUnlock()
	return t.BaseConfig.RetryInterval()
}

func (t *TracerConfig) SetRetryInterval(interval time.Duration, origin telemetry.Origin) {
	t.tmu.Lock()
	t.retryInterval = &interval
	t.tmu.Unlock()
	configtelemetry.Report("DD_TRACE_RETRY_INTERVAL", interval, origin)
}

func (t *TracerConfig) TraceProtocol() float64 {
	t.tmu.RLock()
	if t.traceProtocol != nil {
		v := *t.traceProtocol
		t.tmu.RUnlock()
		return v
	}
	t.tmu.RUnlock()
	return t.BaseConfig.TraceProtocol()
}

func (t *TracerConfig) SetTraceProtocol(v float64, origin telemetry.Origin) {
	t.tmu.Lock()
	t.traceProtocol = &v
	t.tmu.Unlock()
	configtelemetry.Report("DD_TRACE_AGENT_PROTOCOL_VERSION", v, origin)
}

func (t *TracerConfig) OTLPExportMode() bool {
	t.tmu.RLock()
	if t.otlpExportMode != nil {
		v := *t.otlpExportMode
		t.tmu.RUnlock()
		return v
	}
	t.tmu.RUnlock()
	return t.BaseConfig.OTLPExportMode()
}

func (t *TracerConfig) SetOTLPExportMode(v bool, origin telemetry.Origin) {
	t.tmu.Lock()
	t.otlpExportMode = &v
	t.tmu.Unlock()
	configtelemetry.Report("OTEL_TRACES_EXPORTER", v, origin)
}

func (t *TracerConfig) CIVisibilityEnabled() bool {
	t.tmu.RLock()
	if t.ciVisibilityEnabled != nil {
		v := *t.ciVisibilityEnabled
		t.tmu.RUnlock()
		return v
	}
	t.tmu.RUnlock()
	return t.BaseConfig.CIVisibilityEnabled()
}

func (t *TracerConfig) SetCIVisibilityEnabled(enabled bool, origin telemetry.Origin) {
	t.tmu.Lock()
	t.ciVisibilityEnabled = &enabled
	t.tmu.Unlock()
	configtelemetry.Report(constants.CIVisibilityEnabledEnvironmentVariable, enabled, origin)
}
