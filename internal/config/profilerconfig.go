// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2025 Datadog, Inc.

package config

import (
	"sync"
)

// ProfilerConfig holds profiler-specific configuration. It embeds a pointer
// to the shared BaseConfig so shared field accessors are promoted.
//
// Shadow fields will be added here as the profiler's programmatic API
// (e.g. profiler.WithService) is wired through internal/config.
type ProfilerConfig struct {
	*BaseConfig

	pmu sync.RWMutex // protects ProfilerConfig fields only
}

func loadProfilerConfig(g *BaseConfig) *ProfilerConfig {
	return &ProfilerConfig{BaseConfig: g}
}
