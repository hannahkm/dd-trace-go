// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2025 Datadog, Inc.

package config

import (
	"sync"

	configtelemetry "github.com/DataDog/dd-trace-go/v2/internal/config/configtelemetry"
	"github.com/DataDog/dd-trace-go/v2/internal/telemetry"
)

// ProfilerConfig holds profiler-specific configuration. It embeds a pointer
// to the shared BaseConfig so shared field accessors are promoted.
type ProfilerConfig struct {
	*BaseConfig

	pmu sync.RWMutex

	env *string
}

func loadProfilerConfig(g *BaseConfig) *ProfilerConfig {
	return &ProfilerConfig{BaseConfig: g}
}

func (p *ProfilerConfig) Env() string {
	p.pmu.RLock()
	if p.env != nil {
		v := *p.env
		p.pmu.RUnlock()
		return v
	}
	p.pmu.RUnlock()
	return p.BaseConfig.Env()
}

func (p *ProfilerConfig) SetEnv(env string, origin telemetry.Origin) {
	p.pmu.Lock()
	p.env = &env
	p.pmu.Unlock()
	configtelemetry.Report("DD_ENV.profiler", env, origin)
}
