// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016 Datadog, Inc.

package tracer

import (
	"bytes"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/DataDog/dd-trace-go/v2/internal/locking"
	"github.com/DataDog/dd-trace-go/v2/internal/log"
	otlpcommon "go.opentelemetry.io/proto/otlp/common/v1"
	otlpresource "go.opentelemetry.io/proto/otlp/resource/v1"
	otlptrace "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

var _ traceWriter = (*otlpTraceWriter)(nil)

type otlpTraceWriter struct {
	config   *config
	client   *http.Client
	traceURL string
	headers  map[string]string
	mu       locking.Mutex
	resource *otlpresource.Resource
	scope    *otlpcommon.InstrumentationScope
	spans    []*otlptrace.Span // +checklocks:mu
	climit   chan struct{}
	wg       sync.WaitGroup
}

func newOTLPTraceWriter(c *config) *otlpTraceWriter {
	return &otlpTraceWriter{
		config:   c,
		client:   c.httpClient,
		traceURL: c.internalConfig.OTLPTraceURL(),
		headers:  c.internalConfig.OTLPHeaders(),
		resource: buildResource(c.internalConfig),
		scope:    &otlpcommon.InstrumentationScope{Name: "dd-trace-go"},
		spans:    make([]*otlptrace.Span, 0),
		climit:   make(chan struct{}, concurrentConnectionLimit),
		wg:       sync.WaitGroup{},
	}
}

func (w *otlpTraceWriter) add(spanList []*Span) {
	defaultServiceName := w.config.internalConfig.ServiceName()
	w.mu.Lock()
	defer w.mu.Unlock()
	w.spans = slices.Grow(w.spans, len(spanList))
	for _, span := range spanList {
		if otlpSpan := convertSpan(span, defaultServiceName); otlpSpan != nil {
			w.spans = append(w.spans, otlpSpan)
		}
	}
}

// send posts a protobuf-encoded OTLP payload to the configured collector endpoint.
func (w *otlpTraceWriter) send(data []byte) error {
	req, err := http.NewRequest("POST", w.traceURL, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("cannot create http request: %s", err)
	}
	for header, value := range w.headers {
		req.Header.Set(header, value)
	}
	resp, err := w.client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if code := resp.StatusCode; code >= 400 {
		return fmt.Errorf("HTTP %d: %s", code, http.StatusText(code))
	}
	return nil
}

func (w *otlpTraceWriter) flush() {
	w.mu.Lock()
	if len(w.spans) == 0 {
		w.mu.Unlock()
		return
	}
	readySpans := w.spans
	w.spans = make([]*otlptrace.Span, 0)
	w.mu.Unlock()

	w.climit <- struct{}{}
	w.wg.Add(1)
	go func() {
		defer func() {
			<-w.climit
			w.wg.Done()
		}()

		spanCount := len(readySpans)
		tracesData := &otlptrace.TracesData{
			ResourceSpans: []*otlptrace.ResourceSpans{
				{
					Resource: w.resource,
					ScopeSpans: []*otlptrace.ScopeSpans{
						{
							Scope: w.scope,
							Spans: readySpans,
						},
					},
				},
			},
		}
		b, err := proto.Marshal(tracesData)
		readySpans = nil
		tracesData = nil
		if err != nil {
			log.Error("Error marshalling OTLP traces data: %s", err.Error())
			return
		}

		var sendErr error
		for attempt := 0; attempt <= w.config.sendRetries; attempt++ {
			log.Debug("OTLP: attempt %d to send payload: %d bytes, %d spans", attempt+1, len(b), spanCount)
			sendErr = w.send(b)
			if sendErr == nil {
				log.Debug("OTLP: sent traces after %d attempts", attempt+1)
				return
			}
			log.Error("OTLP: failure sending traces (attempt %d of %d): %v", attempt+1, w.config.sendRetries+1, sendErr)
			time.Sleep(w.config.internalConfig.RetryInterval())
		}
		log.Error("OTLP: lost %d spans: %v", spanCount, sendErr)
	}()
}

func (w *otlpTraceWriter) stop() {
	w.flush()
	w.wg.Wait()
}
