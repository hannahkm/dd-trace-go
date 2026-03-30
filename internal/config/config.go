// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2025 Datadog, Inc.

package config

import (
	"sync"

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

var (
	globalInstance *GlobalConfig
	globalMu      sync.Mutex

	// Legacy singleton support for Get() / CreateNew().
	legacyInstance *TracerConfig
	legacyMu       sync.Mutex
	useFreshConfig bool
)

func getGlobalInstance() *GlobalConfig {
	globalMu.Lock()
	defer globalMu.Unlock()
	if globalInstance == nil {
		globalInstance = loadGlobalConfig()
	}
	return globalInstance
}

// GetTracerConfig returns a new TracerConfig pointing at the shared
// GlobalConfig singleton. Each call returns an independent instance
// the caller owns.
func GetTracerConfig() *TracerConfig {
	return loadTracerConfig(getGlobalInstance())
}

// GetProfilerConfig returns the shared GlobalConfig singleton.
// Can be upgraded to return a *ProfilerConfig later when
// profiler-specific fields are added to internal/config.
func GetProfilerConfig() *GlobalConfig {
	return getGlobalInstance()
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
