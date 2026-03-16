// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016 Datadog, Inc.

package internal

import (
	"testing"
)

func TestSpanAttributesZeroValue(t *testing.T) {
	var a SpanAttributes
	for _, key := range []AttrKey{AttrEnv, AttrVersion, AttrComponent, AttrSpanKind, AttrLanguage} {
		if v, ok := a.Get(key); ok || v != "" {
			t.Errorf("key %d: expected absent zero value, got (%q, %v)", key, v, ok)
		}
	}
}

func TestSpanAttributesSetAndGet(t *testing.T) {
	tests := []struct {
		key AttrKey
		val string
	}{
		{AttrEnv, "prod"},
		{AttrVersion, "1.2.3"},
		{AttrComponent, "net/http"},
		{AttrSpanKind, "server"},
		{AttrLanguage, "go"},
	}
	var a SpanAttributes
	for _, tt := range tests {
		a.Set(tt.key, tt.val)
	}
	for _, tt := range tests {
		got, ok := a.Get(tt.key)
		if !ok {
			t.Errorf("key %d: expected present, got absent", tt.key)
		}
		if got != tt.val {
			t.Errorf("key %d: expected %q, got %q", tt.key, tt.val, got)
		}
		if a.Val(tt.key) != tt.val {
			t.Errorf("key %d: Val returned %q, expected %q", tt.key, a.Val(tt.key), tt.val)
		}
	}
}

// Set(key, "") is distinct from never-Set: the bit should be set and value "".
func TestSpanAttributesSetEmptyString(t *testing.T) {
	var a SpanAttributes
	a.Set(AttrEnv, "")
	v, ok := a.Get(AttrEnv)
	if !ok {
		t.Error("expected key to be marked present after Set with empty string")
	}
	if v != "" {
		t.Errorf("expected empty string value, got %q", v)
	}
}

func TestSpanAttributesSetOverwrite(t *testing.T) {
	var a SpanAttributes
	a.Set(AttrEnv, "staging")
	a.Set(AttrEnv, "prod")
	v, ok := a.Get(AttrEnv)
	if !ok {
		t.Error("expected key to be present")
	}
	if v != "prod" {
		t.Errorf("expected overwritten value %q, got %q", "prod", v)
	}
}

func TestSpanAttributesIndependentKeys(t *testing.T) {
	var a SpanAttributes
	a.Set(AttrEnv, "prod")

	// Other keys must remain absent.
	for _, key := range []AttrKey{AttrVersion, AttrComponent, AttrSpanKind, AttrLanguage} {
		if _, ok := a.Get(key); ok {
			t.Errorf("key %d should be absent after setting only AttrEnv", key)
		}
	}
}

func TestSpanAttributesValUnset(t *testing.T) {
	var a SpanAttributes
	// Val on an unset key returns "" without panicking.
	if v := a.Val(AttrVersion); v != "" {
		t.Errorf("expected empty string from unset key, got %q", v)
	}
}

// BenchmarkSpanAttributesSet benchmarks setting all four promoted fields using
// SpanAttributes versus an equivalent map[string]string.
func BenchmarkSpanAttributesSet(b *testing.B) {
	b.Run("SpanAttributes", func(b *testing.B) {
		a := SpanAttributes{}
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			a.Set(AttrEnv, "prod")
			a.Set(AttrVersion, "1.2.3")
			a.Set(AttrComponent, "net/http")
			a.Set(AttrSpanKind, "server")
			a.Set(AttrLanguage, "go")
		}
		_ = a
	})

	b.Run("map", func(b *testing.B) {
		m := make(map[string]string, 4)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			m["env"] = "prod"
			m["version"] = "1.2.3"
			m["component"] = "net/http"
			m["span.kind"] = "server"
		}
		_ = m
	})
}

// BenchmarkSpanAttributesGet benchmarks reading all four promoted fields.
func BenchmarkSpanAttributesGet(b *testing.B) {
	b.Run("SpanAttributes", func(b *testing.B) {
		var a SpanAttributes
		a.Set(AttrEnv, "prod")
		a.Set(AttrVersion, "1.2.3")
		a.Set(AttrComponent, "net/http")
		a.Set(AttrSpanKind, "server")
		a.Set(AttrLanguage, "go")
		b.ReportAllocs()
		b.ResetTimer()
		var s string
		var ok bool
		for i := 0; i < b.N; i++ {
			s, ok = a.Get(AttrEnv)
			s, ok = a.Get(AttrVersion)
			s, ok = a.Get(AttrComponent)
			s, ok = a.Get(AttrSpanKind)
		}
		_, _ = s, ok
	})

	b.Run("map", func(b *testing.B) {
		m := map[string]string{
			"env":       "prod",
			"version":   "1.2.3",
			"component": "net/http",
			"span.kind": "server",
		}
		b.ReportAllocs()
		b.ResetTimer()
		var s string
		var ok bool
		for i := 0; i < b.N; i++ {
			s, ok = m["env"]
			s, ok = m["version"]
			s, ok = m["component"]
			s, ok = m["span.kind"]
		}
		_, _ = s, ok
	})
}
