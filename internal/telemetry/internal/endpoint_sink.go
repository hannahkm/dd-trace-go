// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2026 Datadog, Inc.

package internal

// EndpointSink identifies where a telemetry payload was sent.
type EndpointSink int

const (
	// EndpointSinkAgent identifies a payload sent directly to the agent.
	EndpointSinkAgent EndpointSink = iota
	// EndpointSinkAgentless identifies a payload sent to the agentless telemetry intake.
	EndpointSinkAgentless
	// EndpointSinkFile identifies a payload written to the Bazel undeclared outputs tree.
	EndpointSinkFile
)
