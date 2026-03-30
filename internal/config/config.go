// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2025 Datadog, Inc.

package config

import (
	"fmt"
	"maps"
	"math"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/DataDog/dd-trace-go/v2/internal"
	"github.com/DataDog/dd-trace-go/v2/internal/civisibility/constants"
	configtelemetry "github.com/DataDog/dd-trace-go/v2/internal/config/configtelemetry"
	"github.com/DataDog/dd-trace-go/v2/internal/config/provider"
	"github.com/DataDog/dd-trace-go/v2/internal/env"
	"github.com/DataDog/dd-trace-go/v2/internal/log"
	"github.com/DataDog/dd-trace-go/v2/internal/telemetry"
	"github.com/DataDog/dd-trace-go/v2/internal/traceprof"
)

// Origin represents where a configuration value came from.
// Re-exported so callers don't need to import internal/telemetry.
type Origin = telemetry.Origin

const (
	OriginCode       = telemetry.OriginCode
	OriginCalculated = telemetry.OriginCalculated
	OriginDefault    = telemetry.OriginDefault
)

// ---------------------------------------------------------------------------
// SharedConfig
// ---------------------------------------------------------------------------

// SharedConfig holds all configuration loaded once from init-time config sources (env,
// OTEL, declarative, defaults, RC). A single instance is created lazily and
// shared by all product configs via pointer.
type SharedConfig struct {
	mu sync.RWMutex

	// Universal fields
	agentURL            *url.URL
	debug               bool
	logStartup          bool
	serviceName         string
	version             string
	env                 string
	hostname            string
	hostnameLookupError error
	reportHostname      bool
	logToStdout         bool
	isLambdaFunction    bool
	logDirectory        string
	featureFlags        map[string]struct{}
	logsOTelEnabled     bool

	// Fields without cross-product override conflicts.
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
	dataStreamsMonitoringEnabled  bool
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
	profilingEnabled              bool
}

func loadSharedConfig() *SharedConfig {
	p := provider.New()
	cfg := new(SharedConfig)

	agentURLStr := p.GetString("DD_TRACE_AGENT_URL", "")
	agentHost := p.GetString("DD_AGENT_HOST", "")
	agentPort := p.GetString("DD_TRACE_AGENT_PORT", "")
	cfg.agentURL = resolveAgentURL(agentURLStr, agentHost, agentPort)

	cfg.debug = p.GetBool("DD_TRACE_DEBUG", false)
	cfg.logStartup = p.GetBool("DD_TRACE_STARTUP_LOGS", true)
	cfg.serviceName = p.GetString("DD_SERVICE", "")
	cfg.version = p.GetString("DD_VERSION", "")
	cfg.env = p.GetString("DD_ENV", "")
	cfg.logDirectory = p.GetString("DD_TRACE_LOG_DIRECTORY", "")
	cfg.logsOTelEnabled = p.GetBool("DD_LOGS_OTEL_ENABLED", false)

	cfg.featureFlags = make(map[string]struct{})
	if featuresStr := p.GetString("DD_TRACE_FEATURES", ""); featuresStr != "" {
		for _, feat := range strings.FieldsFunc(featuresStr, func(r rune) bool {
			return r == ',' || r == ' '
		}) {
			cfg.featureFlags[strings.TrimSpace(feat)] = struct{}{}
		}
	}

	if v, ok := env.Lookup("AWS_LAMBDA_FUNCTION_NAME"); ok {
		cfg.logToStdout = true
		if v != "" {
			cfg.isLambdaFunction = true
		}
	}

	if p.GetBool("DD_TRACE_REPORT_HOSTNAME", false) {
		hostname, err := os.Hostname()
		if err != nil {
			log.Warn("unable to look up hostname: %s", err.Error())
			cfg.hostnameLookupError = err
		}
		cfg.hostname = hostname
		cfg.reportHostname = true
	}
	if sourceHostname, ok := env.Lookup("DD_TRACE_SOURCE_HOSTNAME"); ok {
		cfg.hostname = sourceHostname
		cfg.reportHostname = true
	}

	cfg.serviceMappings = p.GetMap("DD_SERVICE_MAPPING", nil, internal.DDTagsDelimiter)
	cfg.runtimeMetrics = p.GetBool("DD_RUNTIME_METRICS_ENABLED", false)
	cfg.runtimeMetricsV2 = p.GetBool("DD_RUNTIME_METRICS_V2_ENABLED", true)
	cfg.profilerHotspots = p.GetBool("DD_PROFILING_CODE_HOTSPOTS_COLLECTION_ENABLED", true)
	cfg.profilerEndpoints = p.GetBool("DD_PROFILING_ENDPOINT_COLLECTION_ENABLED", true)
	cfg.spanAttributeSchemaVersion = p.GetInt("DD_TRACE_SPAN_ATTRIBUTE_SCHEMA", 0)
	cfg.peerServiceDefaultsEnabled = p.GetBool("DD_TRACE_PEER_SERVICE_DEFAULTS_ENABLED", false)
	cfg.peerServiceMappings = p.GetMap("DD_TRACE_PEER_SERVICE_MAPPING", nil, internal.DDTagsDelimiter)
	cfg.debugAbandonedSpans = p.GetBool("DD_TRACE_DEBUG_ABANDONED_SPANS", false)
	cfg.spanTimeout = p.GetDuration("DD_TRACE_ABANDONED_SPAN_TIMEOUT", 10*time.Minute)
	cfg.partialFlushMinSpans = p.GetIntWithValidator("DD_TRACE_PARTIAL_FLUSH_MIN_SPANS", 1000, validatePartialFlushMinSpans)
	cfg.partialFlushEnabled = p.GetBool("DD_TRACE_PARTIAL_FLUSH_ENABLED", false)
	cfg.statsComputationEnabled = p.GetBool("DD_TRACE_STATS_COMPUTATION_ENABLED", true)
	cfg.dataStreamsMonitoringEnabled = p.GetBool("DD_DATA_STREAMS_ENABLED", false)
	cfg.dynamicInstrumentationEnabled = p.GetBool("DD_DYNAMIC_INSTRUMENTATION_ENABLED", false)
	cfg.ciVisibilityEnabled = p.GetBool(constants.CIVisibilityEnabledEnvironmentVariable, false)
	cfg.ciVisibilityAgentless = p.GetBool("DD_CIVISIBILITY_AGENTLESS_ENABLED", false)
	cfg.traceRateLimitPerSecond = p.GetFloatWithValidator("DD_TRACE_RATE_LIMIT", DefaultRateLimit, validateRateLimit)
	cfg.globalSampleRate = p.GetFloatWithValidator("DD_TRACE_SAMPLE_RATE", math.NaN(), validateSampleRate)
	cfg.debugStack = p.GetBool("DD_TRACE_DEBUG_STACK", true)
	cfg.retryInterval = p.GetDuration("DD_TRACE_RETRY_INTERVAL", time.Millisecond)
	cfg.traceProtocol = resolveTraceProtocol(p.GetStringWithValidator("DD_TRACE_AGENT_PROTOCOL_VERSION", TraceProtocolVersionStringV04, validateTraceProtocolVersion))
	cfg.otlpExportMode = p.GetString("OTEL_TRACES_EXPORTER", "") == "otlp"
	if p.IsSet("DD_TRACE_AGENT_PROTOCOL_VERSION") {
		cfg.otlpExportMode = false
	}
	cfg.otlpTraceURL = resolveOTLPTraceURL(cfg.agentURL, p.GetString("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", ""))
	cfg.otlpHeaders = buildOTLPHeaders(p.GetMap("OTEL_EXPORTER_OTLP_TRACES_HEADERS", nil, internal.OtelTagsDelimeter))
	cfg.traceID128BitEnabled = p.GetBool("DD_TRACE_128_BIT_TRACEID_GENERATION_ENABLED", true)

	// DD_PROFILING_ENABLED="auto" means activation is determined by the
	// Datadog admission controller, so treat it as true.
	if v := p.GetString("DD_PROFILING_ENABLED", ""); v == "auto" {
		cfg.profilingEnabled = true
	} else {
		cfg.profilingEnabled = p.GetBool("DD_PROFILING_ENABLED", true)
	}

	return cfg
}

// ---------------------------------------------------------------------------
// Singletons & constructors
// ---------------------------------------------------------------------------

var (
	globalInstance *SharedConfig
	globalMu       sync.Mutex
)

// initGlobal lazily initializes the SharedConfig singleton.
func initGlobal() *SharedConfig {
	globalMu.Lock()
	defer globalMu.Unlock()
	if globalInstance == nil {
		globalInstance = loadSharedConfig()
	}
	return globalInstance
}

// GetSharedConfig returns the shared SharedConfig singleton.
// Use this when you only need shared fields and don't need
// product-specific overrides.
func GetSharedConfig() *SharedConfig {
	return initGlobal()
}

// GetTracerConfig returns a new config for the tracer.
func GetTracerConfig() *TracerConfig {
	return loadTracerConfig(initGlobal())
}

// GetProfilerConfig returns a new config for the profiler.
func GetProfilerConfig() *ProfilerConfig {
	return loadProfilerConfig(initGlobal())
}

// ResetConfig resets the SharedConfig singleton so the next
// access re-reads from environment variables and config sources.
// Intended for use in tests.
func ResetConfig() {
	globalMu.Lock()
	globalInstance = nil
	globalMu.Unlock()
}

// ---------------------------------------------------------------------------
// SharedConfig getters & setters
// ---------------------------------------------------------------------------

func (c *SharedConfig) RawAgentURL() *url.URL {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.agentURL == nil {
		return nil
	}
	u := *c.agentURL
	return &u
}

func (c *SharedConfig) SetAgentURL(u *url.URL, origin telemetry.Origin) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.agentURL = u
	if u != nil {
		configtelemetry.Report("DD_TRACE_AGENT_URL", u.String(), origin)
	}
}

// AgentURL returns the URL to use for HTTP requests to the agent.
// For unix-scheme URLs this rewrites to the http://UDS_... form; otherwise
// it returns a copy of the configured URL.
func (c *SharedConfig) AgentURL() *url.URL {
	u := c.RawAgentURL()
	if u != nil && u.Scheme == "unix" {
		return internal.UnixDataSocketURL(u.Path)
	}
	return u
}

func (c *SharedConfig) Debug() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.debug
}

func (c *SharedConfig) SetDebug(enabled bool, origin telemetry.Origin) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.debug = enabled
	configtelemetry.Report("DD_TRACE_DEBUG", enabled, origin)
}

func (c *SharedConfig) LogStartup() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.logStartup
}

func (c *SharedConfig) SetLogStartup(enabled bool, origin telemetry.Origin) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.logStartup = enabled
	configtelemetry.Report("DD_TRACE_STARTUP_LOGS", enabled, origin)
}

func (c *SharedConfig) ServiceName() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.serviceName
}

func (c *SharedConfig) SetServiceName(name string, origin telemetry.Origin) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.serviceName = name
	configtelemetry.Report("DD_SERVICE", name, origin)
}

func (c *SharedConfig) Version() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.version
}

func (c *SharedConfig) SetVersion(version string, origin telemetry.Origin) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.version = version
	configtelemetry.Report("DD_VERSION", version, origin)
}

func (c *SharedConfig) Env() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.env
}

func (c *SharedConfig) SetEnv(env string, origin telemetry.Origin) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.env = env
	configtelemetry.Report("DD_ENV", env, origin)
}

func (c *SharedConfig) Hostname() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.hostname
}

func (c *SharedConfig) SetHostname(hostname string, origin telemetry.Origin) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.hostname = hostname
	c.reportHostname = true
	configtelemetry.Report("DD_TRACE_SOURCE_HOSTNAME", hostname, origin)
}

func (c *SharedConfig) HostnameLookupError() error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.hostnameLookupError
}

func (c *SharedConfig) ReportHostname() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.reportHostname
}

func (c *SharedConfig) LogToStdout() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.logToStdout
}

func (c *SharedConfig) SetLogToStdout(enabled bool, origin telemetry.Origin) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.logToStdout = enabled
}

func (c *SharedConfig) IsLambdaFunction() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.isLambdaFunction
}

func (c *SharedConfig) SetIsLambdaFunction(enabled bool, origin telemetry.Origin) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.isLambdaFunction = enabled
}

func (c *SharedConfig) LogDirectory() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.logDirectory
}

func (c *SharedConfig) SetLogDirectory(directory string, origin telemetry.Origin) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.logDirectory = directory
	configtelemetry.Report("DD_TRACE_LOG_DIRECTORY", directory, origin)
}

func (c *SharedConfig) SetFeatureFlags(features []string, origin telemetry.Origin) {
	c.mu.Lock()
	if c.featureFlags == nil {
		c.featureFlags = make(map[string]struct{})
	}
	for _, feat := range features {
		c.featureFlags[strings.TrimSpace(feat)] = struct{}{}
	}
	all := make([]string, 0, len(c.featureFlags))
	for feat := range c.featureFlags {
		all = append(all, feat)
	}
	c.mu.Unlock()

	configtelemetry.Report("DD_TRACE_FEATURES", strings.Join(all, ","), origin)
}

func (c *SharedConfig) FeatureFlags() map[string]struct{} {
	c.mu.RLock()
	defer c.mu.RUnlock()
	result := make(map[string]struct{}, len(c.featureFlags))
	maps.Copy(result, c.featureFlags)
	return result
}

// HasFeature performs a single feature flag lookup without copying the map.
func (c *SharedConfig) HasFeature(feat string) bool {
	c.mu.RLock()
	ff := c.featureFlags
	if ff == nil {
		c.mu.RUnlock()
		return false
	}
	_, ok := ff[strings.TrimSpace(feat)]
	c.mu.RUnlock()
	return ok
}

func (c *SharedConfig) LogsOTelEnabled() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.logsOTelEnabled
}

func (c *SharedConfig) SetLogsOTelEnabled(enabled bool, origin telemetry.Origin) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.logsOTelEnabled = enabled
	configtelemetry.Report("DD_LOGS_OTEL_ENABLED", enabled, origin)
}

func (c *SharedConfig) ProfilerEndpoints() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.profilerEndpoints
}

func (c *SharedConfig) SetProfilerEndpoints(enabled bool, origin telemetry.Origin) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.profilerEndpoints = enabled
	configtelemetry.Report("DD_PROFILING_ENDPOINT_COLLECTION_ENABLED", enabled, origin)
}

func (c *SharedConfig) ProfilerHotspotsEnabled() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.profilerHotspots
}

func (c *SharedConfig) SetProfilerHotspotsEnabled(enabled bool, origin telemetry.Origin) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.profilerHotspots = enabled
	configtelemetry.Report(traceprof.CodeHotspotsEnvVar, enabled, origin)
}

func (c *SharedConfig) RuntimeMetricsEnabled() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.runtimeMetrics
}

func (c *SharedConfig) SetRuntimeMetricsEnabled(enabled bool, origin telemetry.Origin) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.runtimeMetrics = enabled
	configtelemetry.Report("DD_RUNTIME_METRICS_ENABLED", enabled, origin)
}

func (c *SharedConfig) RuntimeMetricsV2Enabled() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.runtimeMetricsV2
}

func (c *SharedConfig) SetRuntimeMetricsV2Enabled(enabled bool, origin telemetry.Origin) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.runtimeMetricsV2 = enabled
	configtelemetry.Report("DD_RUNTIME_METRICS_V2_ENABLED", enabled, origin)
}

func (c *SharedConfig) DataStreamsMonitoringEnabled() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.dataStreamsMonitoringEnabled
}

func (c *SharedConfig) SetDataStreamsMonitoringEnabled(enabled bool, origin telemetry.Origin) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dataStreamsMonitoringEnabled = enabled
	configtelemetry.Report("DD_DATA_STREAMS_ENABLED", enabled, origin)
}

func (c *SharedConfig) GlobalSampleRate() float64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.globalSampleRate
}

func (c *SharedConfig) SetGlobalSampleRate(rate float64, origin telemetry.Origin) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.globalSampleRate = rate
	configtelemetry.Report("DD_TRACE_SAMPLE_RATE", rate, origin)
}

func (c *SharedConfig) TraceRateLimitPerSecond() float64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.traceRateLimitPerSecond
}

func (c *SharedConfig) SetTraceRateLimitPerSecond(rate float64, origin telemetry.Origin) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.traceRateLimitPerSecond = rate
	configtelemetry.Report("DD_TRACE_RATE_LIMIT", rate, origin)
}

// PartialFlushEnabled returns the partial flushing configuration under a single read lock.
func (c *SharedConfig) PartialFlushEnabled() (enabled bool, minSpans int) {
	c.mu.RLock()
	enabled = c.partialFlushEnabled
	minSpans = c.partialFlushMinSpans
	c.mu.RUnlock()
	return enabled, minSpans
}

func (c *SharedConfig) SetPartialFlushEnabled(enabled bool, origin telemetry.Origin) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.partialFlushEnabled = enabled
	configtelemetry.Report("DD_TRACE_PARTIAL_FLUSH_ENABLED", enabled, origin)
}

func (c *SharedConfig) SetPartialFlushMinSpans(minSpans int, origin telemetry.Origin) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.partialFlushMinSpans = minSpans
	configtelemetry.Report("DD_TRACE_PARTIAL_FLUSH_MIN_SPANS", minSpans, origin)
}

func (c *SharedConfig) DebugAbandonedSpans() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.debugAbandonedSpans
}

func (c *SharedConfig) SetDebugAbandonedSpans(enabled bool, origin telemetry.Origin) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.debugAbandonedSpans = enabled
	configtelemetry.Report("DD_TRACE_DEBUG_ABANDONED_SPANS", enabled, origin)
}

func (c *SharedConfig) SpanTimeout() time.Duration {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.spanTimeout
}

func (c *SharedConfig) SetSpanTimeout(timeout time.Duration, origin telemetry.Origin) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.spanTimeout = timeout
	configtelemetry.Report("DD_TRACE_ABANDONED_SPAN_TIMEOUT", timeout, origin)
}

func (c *SharedConfig) DebugStack() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.debugStack
}

func (c *SharedConfig) SetDebugStack(enabled bool, origin telemetry.Origin) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.debugStack = enabled
	configtelemetry.Report("DD_TRACE_DEBUG_STACK", enabled, origin)
}

func (c *SharedConfig) StatsComputationEnabled() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.statsComputationEnabled
}

func (c *SharedConfig) SetStatsComputationEnabled(enabled bool, origin telemetry.Origin) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.statsComputationEnabled = enabled
	configtelemetry.Report("DD_TRACE_STATS_COMPUTATION_ENABLED", enabled, origin)
}

// ServiceMappings returns a copy of the service mappings map.
func (c *SharedConfig) ServiceMappings() map[string]string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.serviceMappings == nil {
		return nil
	}
	result := make(map[string]string, len(c.serviceMappings))
	maps.Copy(result, c.serviceMappings)
	return result
}

// ServiceMapping performs a single mapping lookup without copying the map.
func (c *SharedConfig) ServiceMapping(from string) (to string, ok bool) {
	c.mu.RLock()
	m := c.serviceMappings
	if m == nil {
		c.mu.RUnlock()
		return "", false
	}
	to, ok = m[from]
	c.mu.RUnlock()
	return to, ok
}

func (c *SharedConfig) SetServiceMapping(from, to string, origin telemetry.Origin) {
	c.mu.Lock()
	if c.serviceMappings == nil {
		c.serviceMappings = make(map[string]string)
	}
	c.serviceMappings[from] = to
	all := make([]string, 0, len(c.serviceMappings))
	for k, v := range c.serviceMappings {
		all = append(all, fmt.Sprintf("%s:%s", k, v))
	}
	c.mu.Unlock()

	configtelemetry.Report("DD_SERVICE_MAPPING", strings.Join(all, ","), origin)
}

func (c *SharedConfig) RetryInterval() time.Duration {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.retryInterval
}

func (c *SharedConfig) SetRetryInterval(interval time.Duration, origin telemetry.Origin) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.retryInterval = interval
	configtelemetry.Report("DD_TRACE_RETRY_INTERVAL", interval, origin)
}

func (c *SharedConfig) CIVisibilityEnabled() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.ciVisibilityEnabled
}

func (c *SharedConfig) SetCIVisibilityEnabled(enabled bool, origin telemetry.Origin) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ciVisibilityEnabled = enabled
	configtelemetry.Report(constants.CIVisibilityEnabledEnvironmentVariable, enabled, origin)
}

func (c *SharedConfig) TraceProtocol() float64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.traceProtocol
}

func (c *SharedConfig) SetTraceProtocol(v float64, origin telemetry.Origin) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.traceProtocol = v
	configtelemetry.Report("DD_TRACE_AGENT_PROTOCOL_VERSION", v, origin)
}

func (c *SharedConfig) OTLPTraceURL() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.otlpTraceURL
}

func (c *SharedConfig) OTLPExportMode() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.otlpExportMode
}

func (c *SharedConfig) SetOTLPExportMode(v bool, origin telemetry.Origin) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.otlpExportMode = v
	configtelemetry.Report("OTEL_TRACES_EXPORTER", v, origin)
}

// OTLPHeaders returns a copy of the OTLP headers map.
func (c *SharedConfig) OTLPHeaders() map[string]string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return maps.Clone(c.otlpHeaders)
}

func (c *SharedConfig) TraceID128BitEnabled() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.traceID128BitEnabled
}

func (c *SharedConfig) ProfilingEnabled() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.profilingEnabled
}

func (c *SharedConfig) SetProfilingEnabled(enabled bool, origin telemetry.Origin) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.profilingEnabled = enabled
	configtelemetry.Report("DD_PROFILING_ENABLED", enabled, origin)
}
