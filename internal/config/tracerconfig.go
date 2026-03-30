// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2025 Datadog, Inc.

package config

import (
	"fmt"
	"maps"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/DataDog/dd-trace-go/v2/internal"
	"github.com/DataDog/dd-trace-go/v2/internal/civisibility/constants"
	configtelemetry "github.com/DataDog/dd-trace-go/v2/internal/config/configtelemetry"
	"github.com/DataDog/dd-trace-go/v2/internal/config/provider"
	"github.com/DataDog/dd-trace-go/v2/internal/telemetry"
	"github.com/DataDog/dd-trace-go/v2/internal/traceprof"
)

// TracerConfig holds tracer-specific configuration. It embeds a pointer to
// the shared GlobalConfig so global field accessors are promoted. Fields
// that can be overridden per-product via programmatic APIs have local shadow
// fields (nil means "use GlobalConfig value").
type TracerConfig struct {
	*GlobalConfig

	tmu sync.RWMutex // protects TracerConfig fields only

	// Local overrides for global fields. nil means "use GlobalConfig value".
	serviceName *string
	env         *string
	version     *string

	serviceMappings               map[string]string
	runtimeMetrics                bool
	runtimeMetricsV2              bool
	profilerHotspots              bool
	profilerEndpoints             bool
	spanAttributeSchemaVersion    int
	peerServiceDefaultsEnabled    bool
	peerServiceMappings           map[string]string
	debugAbandonedSpans           bool
	spanTimeout                   time.Duration
	partialFlushMinSpans          int
	partialFlushEnabled           bool
	statsComputationEnabled       bool
	dataStreamsMonitoringEnabled   bool
	dynamicInstrumentationEnabled bool
	globalSampleRate              float64
	ciVisibilityEnabled           bool
	ciVisibilityAgentless         bool
	traceRateLimitPerSecond       float64
	debugStack                    bool
	retryInterval                 time.Duration
	traceProtocol                 float64
	otlpExportMode                bool
	otlpTraceURL                  string
	otlpHeaders                   map[string]string
	traceID128BitEnabled          bool
}

func loadTracerConfig(g *GlobalConfig) *TracerConfig {
	tc := &TracerConfig{GlobalConfig: g}
	p := provider.New()

	tc.serviceMappings = p.GetMap("DD_SERVICE_MAPPING", nil, internal.DDTagsDelimiter)
	tc.runtimeMetrics = p.GetBool("DD_RUNTIME_METRICS_ENABLED", false)
	tc.runtimeMetricsV2 = p.GetBool("DD_RUNTIME_METRICS_V2_ENABLED", true)
	tc.profilerHotspots = p.GetBool("DD_PROFILING_CODE_HOTSPOTS_COLLECTION_ENABLED", true)
	tc.profilerEndpoints = p.GetBool("DD_PROFILING_ENDPOINT_COLLECTION_ENABLED", true)
	tc.spanAttributeSchemaVersion = p.GetInt("DD_TRACE_SPAN_ATTRIBUTE_SCHEMA", 0)
	tc.peerServiceDefaultsEnabled = p.GetBool("DD_TRACE_PEER_SERVICE_DEFAULTS_ENABLED", false)
	tc.peerServiceMappings = p.GetMap("DD_TRACE_PEER_SERVICE_MAPPING", nil, internal.DDTagsDelimiter)
	tc.debugAbandonedSpans = p.GetBool("DD_TRACE_DEBUG_ABANDONED_SPANS", false)
	tc.spanTimeout = p.GetDuration("DD_TRACE_ABANDONED_SPAN_TIMEOUT", 10*time.Minute)
	tc.partialFlushMinSpans = p.GetIntWithValidator("DD_TRACE_PARTIAL_FLUSH_MIN_SPANS", 1000, validatePartialFlushMinSpans)
	tc.partialFlushEnabled = p.GetBool("DD_TRACE_PARTIAL_FLUSH_ENABLED", false)
	tc.statsComputationEnabled = p.GetBool("DD_TRACE_STATS_COMPUTATION_ENABLED", true)
	tc.dataStreamsMonitoringEnabled = p.GetBool("DD_DATA_STREAMS_ENABLED", false)
	tc.dynamicInstrumentationEnabled = p.GetBool("DD_DYNAMIC_INSTRUMENTATION_ENABLED", false)
	tc.ciVisibilityEnabled = p.GetBool(constants.CIVisibilityEnabledEnvironmentVariable, false)
	tc.ciVisibilityAgentless = p.GetBool("DD_CIVISIBILITY_AGENTLESS_ENABLED", false)
	tc.traceRateLimitPerSecond = p.GetFloatWithValidator("DD_TRACE_RATE_LIMIT", DefaultRateLimit, validateRateLimit)
	tc.globalSampleRate = p.GetFloatWithValidator("DD_TRACE_SAMPLE_RATE", math.NaN(), validateSampleRate)
	tc.debugStack = p.GetBool("DD_TRACE_DEBUG_STACK", true)
	tc.retryInterval = p.GetDuration("DD_TRACE_RETRY_INTERVAL", time.Millisecond)
	tc.traceProtocol = resolveTraceProtocol(p.GetStringWithValidator("DD_TRACE_AGENT_PROTOCOL_VERSION", TraceProtocolVersionStringV04, validateTraceProtocolVersion))
	tc.otlpExportMode = p.GetString("OTEL_TRACES_EXPORTER", "") == "otlp"
	if p.IsSet("DD_TRACE_AGENT_PROTOCOL_VERSION") {
		tc.otlpExportMode = false
	}
	tc.otlpTraceURL = resolveOTLPTraceURL(g.agentURL, p.GetString("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", ""))
	tc.otlpHeaders = buildOTLPHeaders(p.GetMap("OTEL_EXPORTER_OTLP_TRACES_HEADERS", nil, internal.OtelTagsDelimeter))
	tc.traceID128BitEnabled = p.GetBool("DD_TRACE_128_BIT_TRACEID_GENERATION_ENABLED", true)

	return tc
}

// ---------------------------------------------------------------------------
// Shadow getters (override promoted GlobalConfig methods)
// ---------------------------------------------------------------------------

// ServiceName returns the tracer's local override if set, otherwise the
// global value from config sources.
func (t *TracerConfig) ServiceName() string {
	t.tmu.RLock()
	if t.serviceName != nil {
		name := *t.serviceName
		t.tmu.RUnlock()
		return name
	}
	t.tmu.RUnlock()
	return t.GlobalConfig.ServiceName()
}

// SetServiceName sets a tracer-local override for service name.
func (t *TracerConfig) SetServiceName(name string, origin telemetry.Origin) {
	t.tmu.Lock()
	t.serviceName = &name
	t.tmu.Unlock()
	configtelemetry.Report("DD_SERVICE", name, origin)
}

// Env returns the tracer's local override if set, otherwise the global value.
func (t *TracerConfig) Env() string {
	t.tmu.RLock()
	if t.env != nil {
		e := *t.env
		t.tmu.RUnlock()
		return e
	}
	t.tmu.RUnlock()
	return t.GlobalConfig.Env()
}

// SetEnv sets a tracer-local override for env.
func (t *TracerConfig) SetEnv(env string, origin telemetry.Origin) {
	t.tmu.Lock()
	t.env = &env
	t.tmu.Unlock()
	configtelemetry.Report("DD_ENV", env, origin)
}

// Version returns the tracer's local override if set, otherwise the global value.
func (t *TracerConfig) Version() string {
	t.tmu.RLock()
	if t.version != nil {
		v := *t.version
		t.tmu.RUnlock()
		return v
	}
	t.tmu.RUnlock()
	return t.GlobalConfig.Version()
}

// SetVersion sets a tracer-local override for version.
func (t *TracerConfig) SetVersion(version string, origin telemetry.Origin) {
	t.tmu.Lock()
	t.version = &version
	t.tmu.Unlock()
	configtelemetry.Report("DD_VERSION", version, origin)
}

// ---------------------------------------------------------------------------
// Tracer-specific getters & setters
// ---------------------------------------------------------------------------

func (t *TracerConfig) ProfilerEndpoints() bool {
	t.tmu.RLock()
	defer t.tmu.RUnlock()
	return t.profilerEndpoints
}

func (t *TracerConfig) SetProfilerEndpoints(enabled bool, origin telemetry.Origin) {
	t.tmu.Lock()
	defer t.tmu.Unlock()
	t.profilerEndpoints = enabled
	configtelemetry.Report("DD_PROFILING_ENDPOINT_COLLECTION_ENABLED", enabled, origin)
}

func (t *TracerConfig) ProfilerHotspotsEnabled() bool {
	t.tmu.RLock()
	defer t.tmu.RUnlock()
	return t.profilerHotspots
}

func (t *TracerConfig) SetProfilerHotspotsEnabled(enabled bool, origin telemetry.Origin) {
	t.tmu.Lock()
	defer t.tmu.Unlock()
	t.profilerHotspots = enabled
	configtelemetry.Report(traceprof.CodeHotspotsEnvVar, enabled, origin)
}

func (t *TracerConfig) RuntimeMetricsEnabled() bool {
	t.tmu.RLock()
	defer t.tmu.RUnlock()
	return t.runtimeMetrics
}

func (t *TracerConfig) SetRuntimeMetricsEnabled(enabled bool, origin telemetry.Origin) {
	t.tmu.Lock()
	defer t.tmu.Unlock()
	t.runtimeMetrics = enabled
	configtelemetry.Report("DD_RUNTIME_METRICS_ENABLED", enabled, origin)
}

func (t *TracerConfig) RuntimeMetricsV2Enabled() bool {
	t.tmu.RLock()
	defer t.tmu.RUnlock()
	return t.runtimeMetricsV2
}

func (t *TracerConfig) SetRuntimeMetricsV2Enabled(enabled bool, origin telemetry.Origin) {
	t.tmu.Lock()
	defer t.tmu.Unlock()
	t.runtimeMetricsV2 = enabled
	configtelemetry.Report("DD_RUNTIME_METRICS_V2_ENABLED", enabled, origin)
}

func (t *TracerConfig) DataStreamsMonitoringEnabled() bool {
	t.tmu.RLock()
	defer t.tmu.RUnlock()
	return t.dataStreamsMonitoringEnabled
}

func (t *TracerConfig) SetDataStreamsMonitoringEnabled(enabled bool, origin telemetry.Origin) {
	t.tmu.Lock()
	defer t.tmu.Unlock()
	t.dataStreamsMonitoringEnabled = enabled
	configtelemetry.Report("DD_DATA_STREAMS_ENABLED", enabled, origin)
}

func (t *TracerConfig) GlobalSampleRate() float64 {
	t.tmu.RLock()
	defer t.tmu.RUnlock()
	return t.globalSampleRate
}

func (t *TracerConfig) SetGlobalSampleRate(rate float64, origin telemetry.Origin) {
	t.tmu.Lock()
	defer t.tmu.Unlock()
	t.globalSampleRate = rate
	configtelemetry.Report("DD_TRACE_SAMPLE_RATE", rate, origin)
}

func (t *TracerConfig) TraceRateLimitPerSecond() float64 {
	t.tmu.RLock()
	defer t.tmu.RUnlock()
	return t.traceRateLimitPerSecond
}

func (t *TracerConfig) SetTraceRateLimitPerSecond(rate float64, origin telemetry.Origin) {
	t.tmu.Lock()
	defer t.tmu.Unlock()
	t.traceRateLimitPerSecond = rate
	configtelemetry.Report("DD_TRACE_RATE_LIMIT", rate, origin)
}

// PartialFlushEnabled returns the partial flushing configuration under a single read lock.
func (t *TracerConfig) PartialFlushEnabled() (enabled bool, minSpans int) {
	t.tmu.RLock()
	enabled = t.partialFlushEnabled
	minSpans = t.partialFlushMinSpans
	t.tmu.RUnlock()
	return enabled, minSpans
}

func (t *TracerConfig) SetPartialFlushEnabled(enabled bool, origin telemetry.Origin) {
	t.tmu.Lock()
	defer t.tmu.Unlock()
	t.partialFlushEnabled = enabled
	configtelemetry.Report("DD_TRACE_PARTIAL_FLUSH_ENABLED", enabled, origin)
}

func (t *TracerConfig) SetPartialFlushMinSpans(minSpans int, origin telemetry.Origin) {
	t.tmu.Lock()
	defer t.tmu.Unlock()
	t.partialFlushMinSpans = minSpans
	configtelemetry.Report("DD_TRACE_PARTIAL_FLUSH_MIN_SPANS", minSpans, origin)
}

func (t *TracerConfig) DebugAbandonedSpans() bool {
	t.tmu.RLock()
	defer t.tmu.RUnlock()
	return t.debugAbandonedSpans
}

func (t *TracerConfig) SetDebugAbandonedSpans(enabled bool, origin telemetry.Origin) {
	t.tmu.Lock()
	defer t.tmu.Unlock()
	t.debugAbandonedSpans = enabled
	configtelemetry.Report("DD_TRACE_DEBUG_ABANDONED_SPANS", enabled, origin)
}

func (t *TracerConfig) SpanTimeout() time.Duration {
	t.tmu.RLock()
	defer t.tmu.RUnlock()
	return t.spanTimeout
}

func (t *TracerConfig) SetSpanTimeout(timeout time.Duration, origin telemetry.Origin) {
	t.tmu.Lock()
	defer t.tmu.Unlock()
	t.spanTimeout = timeout
	configtelemetry.Report("DD_TRACE_ABANDONED_SPAN_TIMEOUT", timeout, origin)
}

func (t *TracerConfig) DebugStack() bool {
	t.tmu.RLock()
	defer t.tmu.RUnlock()
	return t.debugStack
}

func (t *TracerConfig) SetDebugStack(enabled bool, origin telemetry.Origin) {
	t.tmu.Lock()
	defer t.tmu.Unlock()
	t.debugStack = enabled
	configtelemetry.Report("DD_TRACE_DEBUG_STACK", enabled, origin)
}

func (t *TracerConfig) StatsComputationEnabled() bool {
	t.tmu.RLock()
	defer t.tmu.RUnlock()
	return t.statsComputationEnabled
}

func (t *TracerConfig) SetStatsComputationEnabled(enabled bool, origin telemetry.Origin) {
	t.tmu.Lock()
	defer t.tmu.Unlock()
	t.statsComputationEnabled = enabled
	configtelemetry.Report("DD_TRACE_STATS_COMPUTATION_ENABLED", enabled, origin)
}

// ServiceMappings returns a copy of the service mappings map.
func (t *TracerConfig) ServiceMappings() map[string]string {
	t.tmu.RLock()
	defer t.tmu.RUnlock()
	if t.serviceMappings == nil {
		return nil
	}
	result := make(map[string]string, len(t.serviceMappings))
	maps.Copy(result, t.serviceMappings)
	return result
}

// ServiceMapping performs a single mapping lookup without copying the map.
func (t *TracerConfig) ServiceMapping(from string) (to string, ok bool) {
	t.tmu.RLock()
	m := t.serviceMappings
	if m == nil {
		t.tmu.RUnlock()
		return "", false
	}
	to, ok = m[from]
	t.tmu.RUnlock()
	return to, ok
}

func (t *TracerConfig) SetServiceMapping(from, to string, origin telemetry.Origin) {
	t.tmu.Lock()
	if t.serviceMappings == nil {
		t.serviceMappings = make(map[string]string)
	}
	t.serviceMappings[from] = to
	all := make([]string, 0, len(t.serviceMappings))
	for k, v := range t.serviceMappings {
		all = append(all, fmt.Sprintf("%s:%s", k, v))
	}
	t.tmu.Unlock()

	configtelemetry.Report("DD_SERVICE_MAPPING", strings.Join(all, ","), origin)
}

func (t *TracerConfig) RetryInterval() time.Duration {
	t.tmu.RLock()
	defer t.tmu.RUnlock()
	return t.retryInterval
}

func (t *TracerConfig) SetRetryInterval(interval time.Duration, origin telemetry.Origin) {
	t.tmu.Lock()
	defer t.tmu.Unlock()
	t.retryInterval = interval
	configtelemetry.Report("DD_TRACE_RETRY_INTERVAL", interval, origin)
}

func (t *TracerConfig) CIVisibilityEnabled() bool {
	t.tmu.RLock()
	defer t.tmu.RUnlock()
	return t.ciVisibilityEnabled
}

func (t *TracerConfig) SetCIVisibilityEnabled(enabled bool, origin telemetry.Origin) {
	t.tmu.Lock()
	defer t.tmu.Unlock()
	t.ciVisibilityEnabled = enabled
	configtelemetry.Report(constants.CIVisibilityEnabledEnvironmentVariable, enabled, origin)
}

func (t *TracerConfig) TraceProtocol() float64 {
	t.tmu.RLock()
	defer t.tmu.RUnlock()
	return t.traceProtocol
}

func (t *TracerConfig) SetTraceProtocol(v float64, origin telemetry.Origin) {
	t.tmu.Lock()
	defer t.tmu.Unlock()
	t.traceProtocol = v
	configtelemetry.Report("DD_TRACE_AGENT_PROTOCOL_VERSION", v, origin)
}

func (t *TracerConfig) OTLPTraceURL() string {
	t.tmu.RLock()
	defer t.tmu.RUnlock()
	return t.otlpTraceURL
}

func (t *TracerConfig) OTLPExportMode() bool {
	t.tmu.RLock()
	defer t.tmu.RUnlock()
	return t.otlpExportMode
}

func (t *TracerConfig) SetOTLPExportMode(v bool, origin telemetry.Origin) {
	t.tmu.Lock()
	defer t.tmu.Unlock()
	t.otlpExportMode = v
	configtelemetry.Report("OTEL_TRACES_EXPORTER", v, origin)
}

// OTLPHeaders returns a copy of the OTLP headers map.
func (t *TracerConfig) OTLPHeaders() map[string]string {
	t.tmu.RLock()
	defer t.tmu.RUnlock()
	return maps.Clone(t.otlpHeaders)
}

func (t *TracerConfig) TraceID128BitEnabled() bool {
	t.tmu.RLock()
	defer t.tmu.RUnlock()
	return t.traceID128BitEnabled
}
