// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2025 Datadog, Inc.

// Package telemetryapi holds the telemetry API surface shared between
// internal/config and internal/telemetry. It exists as a leaf package
// (no imports back into either of those packages) so that both can
// depend on it without creating an import cycle.
package telemetryapi

import "sync"

// Origin describes the source of a configuration change.
type Origin string

const (
	OriginDefault             Origin = "default"
	OriginCode                Origin = "code"
	OriginDDConfig            Origin = "dd_config"
	OriginEnvVar              Origin = "env_var"
	OriginRemoteConfig        Origin = "remote_config"
	OriginLocalStableConfig   Origin = "local_stable_config"
	OriginManagedStableConfig Origin = "fleet_stable_config"
	OriginCalculated          Origin = "calculated"
)

// Namespace describes a product to distinguish telemetry coming from
// different products used by the same application.
type Namespace string

const (
	NamespaceGeneral      Namespace = "general"
	NamespaceTracers      Namespace = "tracers"
	NamespaceProfilers    Namespace = "profilers"
	NamespaceAppSec       Namespace = "appsec"
	NamespaceIAST         Namespace = "iast"
	NamespaceTelemetry    Namespace = "telemetry"
	NamespaceCIVisibility Namespace = "civisibility"
	NamespaceMLObs        Namespace = "mlobs"
	NamespaceRUM          Namespace = "rum"
)

// EmptyID represents the absence of a configuration ID.
const EmptyID = ""

// Configuration is a key-value pair that is used to configure the application.
type Configuration struct {
	Name   string
	Value  any
	Origin Origin
	ID     string
	SeqID  uint64
}

// MetricHandle can be used to submit metric values.
type MetricHandle interface {
	Submit(value float64)
}

var (
	mu                   sync.RWMutex
	registerAppConfigsFn func(kvs ...Configuration)
	countMetricFn        func(namespace Namespace, name string, tags []string) MetricHandle
)

// SetCallbacks registers the telemetry implementation functions. It must be
// called once by internal/telemetry at init time.
func SetCallbacks(
	registerAppConfigs func(kvs ...Configuration),
	countMetric func(namespace Namespace, name string, tags []string) MetricHandle,
) {
	mu.Lock()
	defer mu.Unlock()
	registerAppConfigsFn = registerAppConfigs
	countMetricFn = countMetric
}

// SubmitAppConfigs forwards configuration values to the telemetry client.
// It is a no-op if internal/telemetry has not been imported.
func SubmitAppConfigs(kvs ...Configuration) {
	mu.RLock()
	fn := registerAppConfigsFn
	mu.RUnlock()
	if fn != nil {
		fn(kvs...)
	}
}

// SubmitCount creates a counter metric and submits the given value.
// It is a no-op if internal/telemetry has not been imported.
func SubmitCount(namespace Namespace, name string, tags []string, value float64) {
	mu.RLock()
	fn := countMetricFn
	mu.RUnlock()
	if fn != nil {
		fn(namespace, name, tags).Submit(value)
	}
}
