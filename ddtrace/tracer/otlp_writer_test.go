// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016 Datadog, Inc.

package tracer

import (
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	internalconfig "github.com/DataDog/dd-trace-go/v2/internal/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	otlptrace "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

// mockOTLPSender records sendOTLP calls for test assertions.
type mockOTLPSender struct {
	mu           sync.Mutex
	calls        [][]byte
	failCount    int
	sendAttempts int
}

func (m *mockOTLPSender) sendOTLP(data []byte) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sendAttempts++
	if m.failCount > 0 {
		m.failCount--
		return nil, errors.New("send failed")
	}
	cp := make([]byte, len(data))
	copy(cp, data)
	m.calls = append(m.calls, cp)
	return nil, nil
}

func (m *mockOTLPSender) getSendAttempts() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sendAttempts
}

func (m *mockOTLPSender) getCalls() [][]byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

func newTestOTLPWriter(t *testing.T, sender *mockOTLPSender, opts ...StartOption) *otlpTraceWriter {
	t.Helper()
	cfg, err := newTestConfig(append(opts, func(c *config) {
		c.transport = &simpleTransport{}
	})...)
	require.NoError(t, err)
	return &otlpTraceWriter{
		config:   cfg,
		sender:   sender,
		resource: buildResource(cfg.internalConfig),
		scope:    nil,
		spans:    make([]*otlptrace.Span, 0),
		climit:   make(chan struct{}, concurrentConnectionLimit),
	}
}

func TestOTLPWriterImplementsTraceWriter(t *testing.T) {
	assert.Implements(t, (*traceWriter)(nil), &otlpTraceWriter{})
}

func TestOTLPWriterAdd(t *testing.T) {
	sender := &mockOTLPSender{}
	w := newTestOTLPWriter(t, sender)

	spans := []*Span{
		newSpan("op1", "svc", "res1", 1, 1, 0),
		newSpan("op2", "svc", "res2", 2, 1, 1),
	}
	w.add(spans)

	w.mu.Lock()
	assert.Equal(t, 2, len(w.spans))
	w.mu.Unlock()
}

func TestOTLPWriterAddMultiple(t *testing.T) {
	sender := &mockOTLPSender{}
	w := newTestOTLPWriter(t, sender)

	w.add([]*Span{newSpan("op1", "svc", "res", 1, 1, 0)})
	w.add([]*Span{newSpan("op2", "svc", "res", 2, 1, 0)})
	w.add([]*Span{newSpan("op3", "svc", "res", 3, 1, 0)})

	w.mu.Lock()
	assert.Equal(t, 3, len(w.spans))
	w.mu.Unlock()
}

func TestOTLPWriterFlushEmpty(t *testing.T) {
	sender := &mockOTLPSender{}
	w := newTestOTLPWriter(t, sender)

	w.flush()
	w.wg.Wait()

	assert.Equal(t, 0, sender.getSendAttempts())
}

func TestOTLPWriterFlush(t *testing.T) {
	sender := &mockOTLPSender{}
	w := newTestOTLPWriter(t, sender)

	w.add([]*Span{
		newSpan("op1", "svc", "res", 1, 1, 0),
		newSpan("op2", "svc", "res", 2, 1, 0),
	})
	w.flush()
	w.wg.Wait()

	calls := sender.getCalls()
	require.Equal(t, 1, len(calls))

	var tracesData otlptrace.TracesData
	err := proto.Unmarshal(calls[0], &tracesData)
	require.NoError(t, err)

	rs := tracesData.ResourceSpans
	require.Equal(t, 1, len(rs))
	require.Equal(t, 1, len(rs[0].ScopeSpans))
	assert.Equal(t, 2, len(rs[0].ScopeSpans[0].Spans))
}

func TestOTLPWriterFlushClearsSpans(t *testing.T) {
	sender := &mockOTLPSender{}
	w := newTestOTLPWriter(t, sender)

	w.add([]*Span{newSpan("op1", "svc", "res", 1, 1, 0)})
	w.flush()
	w.wg.Wait()

	w.mu.Lock()
	assert.Equal(t, 0, len(w.spans))
	w.mu.Unlock()

	// Second flush should be a no-op
	w.flush()
	w.wg.Wait()
	assert.Equal(t, 1, sender.getSendAttempts())
}

func TestOTLPWriterFlushRetries(t *testing.T) {
	testcases := []struct {
		configRetries int
		failCount     int
		tracesSent    bool
		expAttempts   int
	}{
		{configRetries: 0, failCount: 0, tracesSent: true, expAttempts: 1},
		{configRetries: 0, failCount: 1, tracesSent: false, expAttempts: 1},

		{configRetries: 1, failCount: 0, tracesSent: true, expAttempts: 1},
		{configRetries: 1, failCount: 1, tracesSent: true, expAttempts: 2},
		{configRetries: 1, failCount: 2, tracesSent: false, expAttempts: 2},

		{configRetries: 2, failCount: 0, tracesSent: true, expAttempts: 1},
		{configRetries: 2, failCount: 1, tracesSent: true, expAttempts: 2},
		{configRetries: 2, failCount: 2, tracesSent: true, expAttempts: 3},
		{configRetries: 2, failCount: 3, tracesSent: false, expAttempts: 3},
	}

	for _, tc := range testcases {
		name := fmt.Sprintf("retries=%d/fails=%d", tc.configRetries, tc.failCount)
		t.Run(name, func(t *testing.T) {
			sender := &mockOTLPSender{failCount: tc.failCount}
			w := newTestOTLPWriter(t, sender, func(c *config) {
				c.sendRetries = tc.configRetries
				c.internalConfig.SetRetryInterval(time.Millisecond, internalconfig.OriginCode)
			})

			w.add([]*Span{newSpan("op", "svc", "res", 1, 1, 0)})
			w.flush()
			w.wg.Wait()

			assert.Equal(t, tc.expAttempts, sender.getSendAttempts())
			assert.Equal(t, tc.tracesSent, len(sender.getCalls()) > 0)
		})
	}
}

func TestOTLPWriterStop(t *testing.T) {
	sender := &mockOTLPSender{}
	w := newTestOTLPWriter(t, sender)

	w.add([]*Span{newSpan("op", "svc", "res", 1, 1, 0)})
	w.stop()

	assert.Equal(t, 1, len(sender.getCalls()))
}

func TestOTLPWriterConcurrency(t *testing.T) {
	sender := &mockOTLPSender{}
	w := newTestOTLPWriter(t, sender)

	const numAdders = 20
	const spansPerAdder = 50
	const numFlushers = 10

	start := make(chan struct{})
	var wg sync.WaitGroup

	var spansAdded int32

	for range numAdders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for range spansPerAdder {
				w.add([]*Span{newSpan("op", "svc", "res", randUint64(), randUint64(), 0)})
				atomic.AddInt32(&spansAdded, 1)
			}
		}()
	}

	for range numFlushers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for range 10 {
				w.flush()
			}
		}()
	}

	close(start)
	wg.Wait()

	w.stop()

	assert.Equal(t, int32(numAdders*spansPerAdder), atomic.LoadInt32(&spansAdded))

	// Verify all sent payloads are valid protobuf
	totalSpans := 0
	for _, data := range sender.getCalls() {
		var td otlptrace.TracesData
		err := proto.Unmarshal(data, &td)
		require.NoError(t, err)
		for _, rs := range td.ResourceSpans {
			for _, ss := range rs.ScopeSpans {
				totalSpans += len(ss.Spans)
			}
		}
	}
	assert.Equal(t, numAdders*spansPerAdder, totalSpans)
}

func TestOTLPWriterNoopSender(t *testing.T) {
	cfg, err := newTestConfig(func(c *config) {
		c.transport = &simpleTransport{}
	})
	require.NoError(t, err)

	w := newOTLPTraceWriter(cfg)

	// simpleTransport doesn't implement otlpSender, so noopOTLPSender should be used
	w.add([]*Span{newSpan("op", "svc", "res", 1, 1, 0)})
	w.flush()
	w.wg.Wait()
	// No panic, no actual send — the noop sender returns an error that gets logged
}
