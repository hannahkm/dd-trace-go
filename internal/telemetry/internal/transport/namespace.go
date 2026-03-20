// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2025 Datadog, Inc.

package transport

import "github.com/DataDog/dd-trace-go/v2/internal/telemetry/telemetryapi"

type Namespace = telemetryapi.Namespace

const (
	NamespaceGeneral      Namespace = telemetryapi.NamespaceGeneral
	NamespaceTracers      Namespace = telemetryapi.NamespaceTracers
	NamespaceProfilers    Namespace = telemetryapi.NamespaceProfilers
	NamespaceAppSec       Namespace = telemetryapi.NamespaceAppSec
	NamespaceIAST         Namespace = telemetryapi.NamespaceIAST
	NamespaceTelemetry    Namespace = telemetryapi.NamespaceTelemetry
	NamespaceCIVisibility Namespace = telemetryapi.NamespaceCIVisibility
	NamespaceMLObs        Namespace = telemetryapi.NamespaceMLObs
	NamespaceRUM          Namespace = telemetryapi.NamespaceRUM
)
