// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2025 Datadog, Inc.

package transport

import "github.com/DataDog/dd-trace-go/v2/internal/telemetry/telemetryapi"

// Origin describes the source of a configuration change
type Origin = telemetryapi.Origin

const (
	OriginDefault             Origin = telemetryapi.OriginDefault
	OriginCode                Origin = telemetryapi.OriginCode
	OriginDDConfig            Origin = telemetryapi.OriginDDConfig
	OriginEnvVar              Origin = telemetryapi.OriginEnvVar
	OriginRemoteConfig        Origin = telemetryapi.OriginRemoteConfig
	OriginLocalStableConfig   Origin = telemetryapi.OriginLocalStableConfig
	OriginManagedStableConfig Origin = telemetryapi.OriginManagedStableConfig
	OriginCalculated          Origin = telemetryapi.OriginCalculated
)
