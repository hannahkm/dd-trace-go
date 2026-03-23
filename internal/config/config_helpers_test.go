// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2025 Datadog, Inc.

package config

import (
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestResolveTraceURL(t *testing.T) {
	httpAgent := &url.URL{Scheme: "http", Host: "myhost:8126"}
	unixAgent := &url.URL{Scheme: "unix", Path: "/var/run/datadog/apm.socket"}

	t.Run("v0.4 protocol uses agent URL", func(t *testing.T) {
		got := resolveTraceURL(TraceProtocolV04, httpAgent, "", "")
		assert.Equal(t, "http://myhost:8126/v0.4/traces", got)
	})

	t.Run("v1 protocol uses agent URL", func(t *testing.T) {
		got := resolveTraceURL(TraceProtocolV1, httpAgent, "", "")
		assert.Equal(t, "http://myhost:8126/v1.0/traces", got)
	})

	t.Run("v0.4 with unix socket rewrites to HTTP", func(t *testing.T) {
		got := resolveTraceURL(TraceProtocolV04, unixAgent, "", "")
		assert.Contains(t, got, "/v0.4/traces")
		assert.Contains(t, got, "http://")
	})

	t.Run("OTLP delegates to resolveOTLPTraceURL", func(t *testing.T) {
		got := resolveTraceURL(TraceProtocolOTLP, httpAgent, "", "")
		assert.Equal(t, "http://myhost:4318/v1/traces", got)
	})

	t.Run("OTLP with traces endpoint set", func(t *testing.T) {
		got := resolveTraceURL(TraceProtocolOTLP, httpAgent, "http://collector:4318/v1/traces", "")
		assert.Equal(t, "http://collector:4318/v1/traces", got)
	})

	t.Run("OTLP with base endpoint set", func(t *testing.T) {
		got := resolveTraceURL(TraceProtocolOTLP, httpAgent, "", "http://collector:4318")
		assert.Equal(t, "http://collector:4318/v1/traces", got)
	})
}

func TestResolveOTLPTraceURL(t *testing.T) {
	httpAgent := &url.URL{Scheme: "http", Host: "myhost:8126"}

	t.Run("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT takes priority", func(t *testing.T) {
		got := resolveOTLPTraceURL(httpAgent, "http://traces-collector:4318/v1/traces", "http://base-collector:4318")
		assert.Equal(t, "http://traces-collector:4318/v1/traces", got)
	})

	t.Run("OTEL_EXPORTER_OTLP_ENDPOINT used as base with path appended", func(t *testing.T) {
		got := resolveOTLPTraceURL(httpAgent, "", "http://collector:4318")
		assert.Equal(t, "http://collector:4318/v1/traces", got)
	})

	t.Run("OTEL_EXPORTER_OTLP_ENDPOINT trailing slash is trimmed", func(t *testing.T) {
		got := resolveOTLPTraceURL(httpAgent, "", "http://collector:4318/")
		assert.Equal(t, "http://collector:4318/v1/traces", got)
	})

	t.Run("default uses agent host with OTLP port", func(t *testing.T) {
		got := resolveOTLPTraceURL(httpAgent, "", "")
		assert.Equal(t, "http://myhost:4318/v1/traces", got)
	})

	t.Run("default with nil agent URL uses localhost", func(t *testing.T) {
		got := resolveOTLPTraceURL(nil, "", "")
		assert.Equal(t, "http://localhost:4318/v1/traces", got)
	})

	t.Run("default with unix socket agent uses localhost", func(t *testing.T) {
		unixAgent := &url.URL{Scheme: "unix", Path: "/var/run/datadog/apm.socket"}
		got := resolveOTLPTraceURL(unixAgent, "", "")
		assert.Equal(t, "http://localhost:4318/v1/traces", got)
	})
}

func TestResolveOTLPHeaders(t *testing.T) {
	t.Run("TRACES_HEADERS takes priority over HEADERS", func(t *testing.T) {
		got := resolveOTLPHeaders("api-key=traces-key", "api-key=generic-key")
		assert.Equal(t, "traces-key", got["api-key"])
	})

	t.Run("falls back to HEADERS when TRACES_HEADERS is empty", func(t *testing.T) {
		got := resolveOTLPHeaders("", "api-key=fallback")
		assert.Equal(t, "fallback", got["api-key"])
	})

	t.Run("always includes Content-Type for protobuf", func(t *testing.T) {
		got := resolveOTLPHeaders("", "")
		assert.Equal(t, OTLPContentTypeHeader, got["Content-Type"])
	})

	t.Run("user headers plus Content-Type", func(t *testing.T) {
		got := resolveOTLPHeaders("api-key=key,other=value", "")
		assert.Equal(t, "key", got["api-key"])
		assert.Equal(t, "value", got["other"])
		assert.Equal(t, OTLPContentTypeHeader, got["Content-Type"])
	})

}

func TestParseOTLPHeaders(t *testing.T) {
	t.Run("parses comma-separated key=value pairs", func(t *testing.T) {
		got := parseOTLPHeaders("api-key=key,other-config=value")
		assert.Equal(t, map[string]string{"api-key": "key", "other-config": "value"}, got)
	})

	t.Run("trims whitespace around keys and values", func(t *testing.T) {
		got := parseOTLPHeaders(" api-key = key , other = value ")
		assert.Equal(t, map[string]string{"api-key": "key", "other": "value"}, got)
	})

	t.Run("skips empty pairs", func(t *testing.T) {
		got := parseOTLPHeaders("api-key=key,,other=value,")
		assert.Equal(t, map[string]string{"api-key": "key", "other": "value"}, got)
	})

	t.Run("skips pairs without =", func(t *testing.T) {
		got := parseOTLPHeaders("valid=yes,invalid,also-valid=ok")
		assert.Equal(t, map[string]string{"valid": "yes", "also-valid": "ok"}, got)
	})

	t.Run("empty string returns empty map", func(t *testing.T) {
		got := parseOTLPHeaders("")
		assert.Empty(t, got)
	})

	t.Run("value can contain equals sign", func(t *testing.T) {
		got := parseOTLPHeaders("auth=base64encoded==")
		assert.Equal(t, "base64encoded==", got["auth"])
	})
}
