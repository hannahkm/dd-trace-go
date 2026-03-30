// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2025 Datadog, Inc.

package config

import (
	"sync"

	configtelemetry "github.com/DataDog/dd-trace-go/v2/internal/config/configtelemetry"
	"github.com/DataDog/dd-trace-go/v2/internal/config/provider"
	"github.com/DataDog/dd-trace-go/v2/internal/telemetry"
)

// ProfilerConfig holds profiler-specific configuration. It embeds a pointer
// to the shared GlobalConfig so global field accessors are promoted.
type ProfilerConfig struct {
	*GlobalConfig

	pmu sync.RWMutex // protects ProfilerConfig fields only

	enabled bool
}

func loadProfilerConfig(g *GlobalConfig, p *provider.Provider) *ProfilerConfig {
	pc := &ProfilerConfig{GlobalConfig: g}

	// DD_PROFILING_ENABLED="auto" means activation is determined by the
	// Datadog admission controller, so treat it as true.
	if v := p.GetString("DD_PROFILING_ENABLED", ""); v == "auto" {
		pc.enabled = true
	} else {
		pc.enabled = p.GetBool("DD_PROFILING_ENABLED", true)
	}

	return pc
}

// ---------------------------------------------------------------------------
// ProfilerConfig getters & setters
// ---------------------------------------------------------------------------

func (p *ProfilerConfig) Enabled() bool {
	p.pmu.RLock()
	defer p.pmu.RUnlock()
	return p.enabled
}

func (p *ProfilerConfig) SetEnabled(enabled bool, origin telemetry.Origin) {
	p.pmu.Lock()
	defer p.pmu.Unlock()
	p.enabled = enabled
	configtelemetry.Report("DD_PROFILING_ENABLED", enabled, origin)
}
