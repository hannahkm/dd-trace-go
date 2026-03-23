// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2025 Datadog, Inc.

package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"

	"github.com/DataDog/dd-trace-go/v2/internal"
	"github.com/DataDog/dd-trace-go/v2/internal/log"
)

const (
	// DefaultRateLimit specifies the default rate limit per second for traces.
	// TODO: Maybe delete this. We will have defaults in supported_configuration.json anyway.
	DefaultRateLimit = 100.0
	// TraceMaxSize is the maximum number of spans we keep in memory for a
	// single trace. This is to avoid memory leaks. If more spans than this
	// are added to a trace, then the trace is dropped and the spans are
	// discarded. Adding additional spans after a trace is dropped does
	// nothing.
	TraceMaxSize = int(1e5)

	// Datadog trace protocol versions (agent wire format).
	TraceProtocolV04              = 0.4 // default
	TraceProtocolV1               = 1.0
	TraceProtocolVersionStringV04 = "0.4"
	TraceProtocolVersionStringV1  = "1.0"

	// Agent URL schemes supported by DD_TRACE_AGENT_URL.
	URLSchemeUnix  = "unix"
	URLSchemeHTTP  = "http"
	URLSchemeHTTPS = "https"

	// Trace API paths appended to the agent URL for each protocol.
	TracesPathV04 = "/v0.4/traces"
	TracesPathV1  = "/v1.0/traces"

	// OTLP standard traces path and default collector port.
	otlpTracesPath  = "/v1/traces"
	otlpDefaultPort = "4318"

	// OTLPContentTypeHeader is the Content-Type header value required for HTTP protobuf payloads.
	OTLPContentTypeHeader = "application/x-protobuf"
)

func validateSampleRate(rate float64) bool {
	if rate < 0.0 || rate > 1.0 {
		log.Warn("ignoring DD_TRACE_SAMPLE_RATE: out of range %f", rate)
		return false
	}
	return true
}

func validateRateLimit(rate float64) bool {
	if rate < 0.0 {
		log.Warn("ignoring DD_TRACE_RATE_LIMIT: negative value %f", rate)
		return false
	}
	return true
}

func validatePartialFlushMinSpans(minSpans int) bool {
	if minSpans <= 0 {
		log.Warn("ignoring DD_TRACE_PARTIAL_FLUSH_MIN_SPANS: negative value %d", minSpans)
		return false
	}
	if minSpans >= TraceMaxSize {
		log.Warn("ignoring DD_TRACE_PARTIAL_FLUSH_MIN_SPANS: value %d is greater than the max number of spans that can be kept in memory for a single trace (%d spans)", minSpans, TraceMaxSize)
		return false
	}
	return true
}

func validateTraceProtocolVersion(v string) bool {
	return v == TraceProtocolVersionStringV04 || v == TraceProtocolVersionStringV1
}

func resolveTraceProtocol(v string) float64 {
	if v == TraceProtocolVersionStringV1 {
		return TraceProtocolV1
	}
	return TraceProtocolV04
}

// resolveAgentURL computes the final agent URL from the three env-var strings
// read through the provider. The priority mirrors internal.AgentURLFromEnv:
//  1. DD_TRACE_AGENT_URL (if non-empty and valid)
//  2. DD_AGENT_HOST / DD_TRACE_AGENT_PORT (if either is non-empty)
//  3. DefaultTraceAgentUDSPath (if the socket file exists)
//  4. http://localhost:8126
func resolveAgentURL(agentURLStr, host, port string) *url.URL {
	if agentURLStr != "" {
		u, err := url.Parse(agentURLStr)
		if err == nil {
			switch u.Scheme {
			case URLSchemeUnix, URLSchemeHTTP, URLSchemeHTTPS:
				return u
			default:
				log.Warn("Unsupported protocol %q in Agent URL %q. Must be one of: %s, %s, %s.", u.Scheme, agentURLStr, URLSchemeHTTP, URLSchemeHTTPS, URLSchemeUnix)
			}
		} else {
			log.Warn("Failed to parse DD_TRACE_AGENT_URL: %s", err.Error())
		}
	}

	httpURL := buildHTTPURL(host, port)
	// If either the host or port is set, return the HTTP URL, else try to detect the UDS URL
	if host != "" || port != "" {
		return httpURL
	}
	if u := detectUDSURL(); u != nil {
		return u
	}
	return httpURL
}

func buildHTTPURL(host, port string) *url.URL {
	if host == "" {
		host = internal.DefaultAgentHostname
	}
	if port == "" {
		port = internal.DefaultTraceAgentPort
	}
	return &url.URL{
		Scheme: URLSchemeHTTP,
		Host:   net.JoinHostPort(host, port),
	}
}

func detectUDSURL() *url.URL {
	if _, err := os.Stat(internal.DefaultTraceAgentUDSPath); err != nil {
		return nil
	}
	return &url.URL{
		Scheme: URLSchemeUnix,
		Path:   internal.DefaultTraceAgentUDSPath,
	}
}

// resolveTraceURL computes the full trace endpoint URL. In OTLP export mode
// it uses the OTLP endpoint values; otherwise it derives the URL from the
// agent URL and Datadog protocol version.
func resolveTraceURL(otlpMode bool, protocol float64, rawAgentURL *url.URL, otlpTracesEndpoint, otlpEndpoint string) string {
	if otlpMode {
		return resolveOTLPTraceURL(rawAgentURL, otlpTracesEndpoint, otlpEndpoint)
	}
	agentHTTPURL := rawAgentURL
	if rawAgentURL != nil && rawAgentURL.Scheme == URLSchemeUnix {
		agentHTTPURL = internal.UnixDataSocketURL(rawAgentURL.Path)
	}
	if protocol == TraceProtocolV1 {
		return agentHTTPURL.String() + TracesPathV1
	}
	return agentHTTPURL.String() + TracesPathV04
}

// resolveOTLPTraceURL resolves the OTLP trace endpoint using the following
// OTel env-var priority:
//  1. OTEL_EXPORTER_OTLP_TRACES_ENDPOINT (full URL)
//  2. OTEL_EXPORTER_OTLP_ENDPOINT (base URL) + /v1/traces
//  3. agentURL host + default OTLP port 4318 + /v1/traces
func resolveOTLPTraceURL(rawAgentURL *url.URL, otlpTracesEndpoint, otlpEndpoint string) string {
	if otlpTracesEndpoint != "" {
		return otlpTracesEndpoint
	}
	if otlpEndpoint != "" {
		return strings.TrimRight(otlpEndpoint, "/") + otlpTracesPath
	}
	host := internal.DefaultAgentHostname
	if rawAgentURL != nil {
		if h := rawAgentURL.Hostname(); h != "" {
			host = h
		}
	}
	return fmt.Sprintf("http://%s:%s%s", host, otlpDefaultPort, otlpTracesPath)
}

// resolveOTLPHeaders builds the OTLP trace headers map from
// OTEL_EXPORTER_OTLP_TRACES_HEADERS. The required Content-Type header for
// HTTP protobuf is always included.
func resolveOTLPHeaders(otlpTracesHeaders string) map[string]string {
	headers := make(map[string]string)
	internal.ForEachStringTag(otlpTracesHeaders, "=", func(k, v string) { headers[k] = v })
	headers["Content-Type"] = OTLPContentTypeHeader
	return headers
}
