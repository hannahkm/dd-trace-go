// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016 Datadog, Inc.

package tracer

import (
	"fmt"
	"io"
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
	sender   otlpSender
	mu       locking.Mutex
	resource *otlpresource.Resource
	scope    *otlpcommon.InstrumentationScope
	spans    []*otlptrace.Span // +checklocks:mu
	climit   chan struct{}
	wg       sync.WaitGroup
}

// noopOTLPSender is a fallback sender that drops all payloads. Used when the
// configured transport does not implement otlpSender.
type noopOTLPSender struct{}

func (noopOTLPSender) sendOTLP([]byte) (io.ReadCloser, error) {
	return nil, fmt.Errorf("OTLP sending is not supported by the configured transport")
}

func newOTLPTraceWriter(c *config) *otlpTraceWriter {
	sender, ok := c.transport.(otlpSender)
	if !ok {
		log.Error("OTLP trace writer: transport %T does not implement otlpSender; traces will be dropped", c.transport)
		sender = noopOTLPSender{}
	}
	return &otlpTraceWriter{
		config:   c,
		sender:   sender,
		resource: buildResource(c.internalConfig),
		scope:    &otlpcommon.InstrumentationScope{Name: "dd-trace-go"},
		spans:    make([]*otlptrace.Span, 0),
		climit:   make(chan struct{}, concurrentConnectionLimit),
		wg:       sync.WaitGroup{},
	}
}

func (w *otlpTraceWriter) add(spanList []*Span) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.spans = slices.Grow(w.spans, len(spanList))
	for _, span := range spanList {
		w.spans = append(w.spans, convertSpan(span))
	}
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
			_, sendErr = w.sender.sendOTLP(b)
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
	// TODO: agentTraceWriter reports datadog.tracer.flush_triggered to its statsd client here; do we want to do the same here?
	w.flush()
	w.wg.Wait()
}
