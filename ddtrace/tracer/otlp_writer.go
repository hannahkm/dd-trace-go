// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016 Datadog, Inc.

package tracer

import (
	"slices"
	"sync"

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
		resource: buildResource(c),
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
	defer w.mu.Unlock()
	tracesData := &otlptrace.TracesData{
		ResourceSpans: []*otlptrace.ResourceSpans{
			{
				Resource: w.resource,
				ScopeSpans: []*otlptrace.ScopeSpans{
					{
						Scope: w.scope,
						Spans: w.spans,
					},
				},
			},
		},
	}
	_, err := proto.Marshal(tracesData)
	if err != nil {
		log.Error("Error marshalling OTLP traces data: %s", err.Error())
		return
	}
	// w.climit <- struct{}{}
	// w.wg.Add(1)
	// go func() {
	// 	defer w.wg.Done()
	// 	<-w.climit
	// 	w.config.transport.send(b)
	// }()
}

func (w *otlpTraceWriter) stop() {
	// TODO: agentTraceWriter reports datadog.tracer.flush_triggered to its statsd client here; do we want to do the same here?
	w.flush()
	w.wg.Wait()
}
