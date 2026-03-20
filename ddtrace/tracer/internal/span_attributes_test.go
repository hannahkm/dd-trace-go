// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016 Datadog, Inc.

package internal

import (
	"maps"
	"testing"
)

func TestSpanAttributesZeroValue(t *testing.T) {
	var a SpanAttributes
	for _, key := range []AttrKey{AttrEnv, AttrVersion, AttrComponent, AttrSpanKind} {
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
	for _, key := range []AttrKey{AttrVersion, AttrComponent, AttrSpanKind} {
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

func TestSpanAttributesForEach(t *testing.T) {
	var a SpanAttributes
	a.Set(AttrEnv, "prod")
	a.Set(AttrSpanKind, "server")
	// AttrVersion and AttrComponent are NOT set

	got := maps.Collect(a.All())
	if len(got) != 2 {
		t.Fatalf("expected 2 entries, got %d: %v", len(got), got)
	}
	if got["env"] != "prod" {
		t.Errorf("expected env=prod, got %q", got["env"])
	}
	if got["span.kind"] != "server" {
		t.Errorf("expected span.kind=server, got %q", got["span.kind"])
	}
}

func TestSpanAttributesForEachNil(t *testing.T) {
	var a *SpanAttributes
	called := false
	for range a.All() {
		called = true
	}
	if called {
		t.Error("All() should not call fn on nil receiver")
	}
}

func TestAttrKeyForTag(t *testing.T) {
	tests := []struct {
		tag string
		key AttrKey
		ok  bool
	}{
		{"env", AttrEnv, true},
		{"version", AttrVersion, true},
		{"component", AttrComponent, true},
		{"span.kind", AttrSpanKind, true},
		{"unknown", AttrUnknown, false},
		{"", AttrUnknown, false},
	}
	for _, tt := range tests {
		key, ok := AttrKeyForTag(tt.tag)
		if ok != tt.ok || key != tt.key {
			t.Errorf("AttrKeyForTag(%q) = (%d, %v), want (%d, %v)", tt.tag, key, ok, tt.key, tt.ok)
		}
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

// TestSpanMetaSetPromotedEmptyString verifies that Set("env", "") on a span with
// no prior env records the key as present (presence bit set), rather than
// silently no-oping because Val() returns "" for unset keys.
func TestSpanMetaSetPromotedEmptyString(t *testing.T) {
	sm := NewSpanMeta(nil)
	sm.Set("env", "")
	v, ok := sm.Get("env")
	if !ok {
		t.Fatal("expected env to be present after Set(\"env\", \"\"), got absent")
	}
	if v != "" {
		t.Fatalf("expected empty string, got %q", v)
	}
}

// TestSpanMetaSetPromotedNoOpWhenPresent verifies that Set("env", value) when
// env is already set to the same value leaves the value unchanged, and that
// updating to a different value is observed correctly.
func TestSpanMetaSetPromotedNoOpWhenPresent(t *testing.T) {
	var a SpanAttributes
	a.Set(AttrEnv, "prod")
	a.MarkShared()
	sm := NewSpanMeta(&a)

	// Same value: result must still be ("prod", true).
	sm.Set("env", "prod")
	v, ok := sm.Get("env")
	if !ok || v != "prod" {
		t.Fatalf("no-op case: expected (prod, true), got (%q, %v)", v, ok)
	}

	// Different value: must be updated.
	sm.Set("env", "staging")
	v, ok = sm.Get("env")
	if !ok || v != "staging" {
		t.Fatalf("update case: expected (staging, true), got (%q, %v)", v, ok)
	}
}

// BenchmarkMerge measures the allocation cost of Merge() before and after Inline().
//
// Before Inline(), Merge() must allocate a fresh map to combine m and attrs.
// After Inline(), all promoted attrs are already in m and Merge() returns sm.m
// directly — zero allocation.
func BenchmarkMerge(b *testing.B) {
	newMeta := func() SpanMeta {
		var a SpanAttributes
		a.Set(AttrEnv, "prod")
		a.Set(AttrVersion, "1.2.3")
		a.Set(AttrComponent, "net/http")
		a.Set(AttrSpanKind, "server")
		sm := NewSpanMeta(&a)
		sm.SetMap("key0", "value0")
		sm.SetMap("key1", "value1")
		sm.SetMap("key2", "value2")
		sm.SetMap("key3", "value3")
		sm.SetMap("key4", "value4")
		return sm
	}

	b.Run("before-Inline", func(b *testing.B) {
		sm := newMeta()
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			_ = sm.Merge()
		}
	})

	b.Run("after-Inline", func(b *testing.B) {
		sm := newMeta()
		sm.Inline()
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			_ = sm.Merge()
		}
	})
}
