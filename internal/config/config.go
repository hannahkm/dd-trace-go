// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2025 Datadog, Inc.

package config

import (
	"maps"
	"net/url"
	"os"
	"strings"
	"sync"

	"github.com/DataDog/dd-trace-go/v2/internal"
	configtelemetry "github.com/DataDog/dd-trace-go/v2/internal/config/configtelemetry"
	"github.com/DataDog/dd-trace-go/v2/internal/config/provider"
	"github.com/DataDog/dd-trace-go/v2/internal/env"
	"github.com/DataDog/dd-trace-go/v2/internal/log"
	"github.com/DataDog/dd-trace-go/v2/internal/telemetry"
)

// Origin represents where a configuration value came from.
// Re-exported so callers don't need to import internal/telemetry.
type Origin = telemetry.Origin

// Re-exported origin constants for common configuration sources
const (
	OriginCode       = telemetry.OriginCode
	OriginCalculated = telemetry.OriginCalculated
	OriginDefault    = telemetry.OriginDefault
)

// Config is a type alias for TracerConfig, kept for backward compatibility.
// New code should use TracerConfig or GlobalConfig directly.
type Config = TracerConfig

// ---------------------------------------------------------------------------
// GlobalConfig
// ---------------------------------------------------------------------------

// GlobalConfig holds configuration shared across all products.
// A single instance is loaded from config sources (env, OTEL, declarative,
// defaults) and shared by all product configs via pointer.
type GlobalConfig struct {
	mu sync.RWMutex

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
}

func loadGlobalConfig(p *provider.Provider) *GlobalConfig {
	cfg := new(GlobalConfig)

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

	return cfg
}

// ---------------------------------------------------------------------------
// Singletons & constructors
// ---------------------------------------------------------------------------

var (
	globalInstance  *GlobalConfig
	globalProvider *provider.Provider
	globalMu       sync.Mutex

	// Legacy singleton support for Get() / CreateNew().
	legacyInstance *TracerConfig
	legacyMu       sync.Mutex
	useFreshConfig bool
)

// initGlobal lazily initializes the GlobalConfig singleton and its
// provider. Returns both so product constructors can reuse the same
// provider (avoiding re-parsing declarative config YAML).
func initGlobal() (*GlobalConfig, *provider.Provider) {
	globalMu.Lock()
	defer globalMu.Unlock()
	if globalInstance == nil {
		globalProvider = provider.New()
		globalInstance = loadGlobalConfig(globalProvider)
	}
	return globalInstance, globalProvider
}

// GetTracerConfig returns a new TracerConfig pointing at the shared
// GlobalConfig singleton. Each call returns an independent instance
// the caller owns.
func GetTracerConfig() *TracerConfig {
	g, p := initGlobal()
	return loadTracerConfig(g, p)
}

// GetProfilerConfig returns a new ProfilerConfig pointing at the shared
// GlobalConfig singleton. Each call returns an independent instance
// the caller owns.
func GetProfilerConfig() *ProfilerConfig {
	g, p := initGlobal()
	return loadProfilerConfig(g, p)
}

// Get returns the legacy TracerConfig singleton.
// New code should use GetTracerConfig() instead.
func Get() *Config {
	legacyMu.Lock()
	defer legacyMu.Unlock()
	if useFreshConfig || legacyInstance == nil {
		legacyInstance = GetTracerConfig()
	}
	return legacyInstance
}

// CreateNew creates a fresh TracerConfig and replaces the legacy singleton.
// New code should use GetTracerConfig() instead.
func CreateNew() *Config {
	legacyMu.Lock()
	defer legacyMu.Unlock()
	legacyInstance = GetTracerConfig()
	return legacyInstance
}

func SetUseFreshConfig(use bool) {
	legacyMu.Lock()
	defer legacyMu.Unlock()
	useFreshConfig = use
}

// ---------------------------------------------------------------------------
// GlobalConfig getters & setters
// ---------------------------------------------------------------------------

func (c *GlobalConfig) RawAgentURL() *url.URL {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.agentURL == nil {
		return nil
	}
	u := *c.agentURL
	return &u
}

func (c *GlobalConfig) SetAgentURL(u *url.URL, origin telemetry.Origin) {
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
func (c *GlobalConfig) AgentURL() *url.URL {
	u := c.RawAgentURL()
	if u != nil && u.Scheme == "unix" {
		return internal.UnixDataSocketURL(u.Path)
	}
	return u
}

func (c *GlobalConfig) Debug() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.debug
}

func (c *GlobalConfig) SetDebug(enabled bool, origin telemetry.Origin) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.debug = enabled
	configtelemetry.Report("DD_TRACE_DEBUG", enabled, origin)
}

func (c *GlobalConfig) LogStartup() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.logStartup
}

func (c *GlobalConfig) SetLogStartup(enabled bool, origin telemetry.Origin) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.logStartup = enabled
	configtelemetry.Report("DD_TRACE_STARTUP_LOGS", enabled, origin)
}

func (c *GlobalConfig) ServiceName() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.serviceName
}

func (c *GlobalConfig) SetServiceName(name string, origin telemetry.Origin) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.serviceName = name
	configtelemetry.Report("DD_SERVICE", name, origin)
}

func (c *GlobalConfig) Version() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.version
}

func (c *GlobalConfig) SetVersion(version string, origin telemetry.Origin) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.version = version
	configtelemetry.Report("DD_VERSION", version, origin)
}

func (c *GlobalConfig) Env() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.env
}

func (c *GlobalConfig) SetEnv(env string, origin telemetry.Origin) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.env = env
	configtelemetry.Report("DD_ENV", env, origin)
}

func (c *GlobalConfig) Hostname() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.hostname
}

func (c *GlobalConfig) SetHostname(hostname string, origin telemetry.Origin) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.hostname = hostname
	c.reportHostname = true
	configtelemetry.Report("DD_TRACE_SOURCE_HOSTNAME", hostname, origin)
}

func (c *GlobalConfig) HostnameLookupError() error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.hostnameLookupError
}

func (c *GlobalConfig) ReportHostname() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.reportHostname
}

func (c *GlobalConfig) LogToStdout() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.logToStdout
}

func (c *GlobalConfig) SetLogToStdout(enabled bool, origin telemetry.Origin) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.logToStdout = enabled
}

func (c *GlobalConfig) IsLambdaFunction() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.isLambdaFunction
}

func (c *GlobalConfig) SetIsLambdaFunction(enabled bool, origin telemetry.Origin) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.isLambdaFunction = enabled
}

func (c *GlobalConfig) LogDirectory() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.logDirectory
}

func (c *GlobalConfig) SetLogDirectory(directory string, origin telemetry.Origin) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.logDirectory = directory
	configtelemetry.Report("DD_TRACE_LOG_DIRECTORY", directory, origin)
}

func (c *GlobalConfig) SetFeatureFlags(features []string, origin telemetry.Origin) {
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

func (c *GlobalConfig) FeatureFlags() map[string]struct{} {
	c.mu.RLock()
	defer c.mu.RUnlock()
	result := make(map[string]struct{}, len(c.featureFlags))
	maps.Copy(result, c.featureFlags)
	return result
}

// HasFeature performs a single feature flag lookup without copying the map.
func (c *GlobalConfig) HasFeature(feat string) bool {
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

func (c *GlobalConfig) LogsOTelEnabled() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.logsOTelEnabled
}

func (c *GlobalConfig) SetLogsOTelEnabled(enabled bool, origin telemetry.Origin) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.logsOTelEnabled = enabled
	configtelemetry.Report("DD_LOGS_OTEL_ENABLED", enabled, origin)
}
