// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016 Datadog, Inc.

package internal

import (
	"iter"
	"math/bits"
)

// AttrKey is an integer index into a SpanAttributes value array.
// Use the pre-declared constants; do not construct AttrKey from arbitrary integers.
type AttrKey uint8

const (
	AttrEnv       AttrKey = 0
	AttrVersion   AttrKey = 1
	AttrComponent AttrKey = 2
	AttrSpanKind  AttrKey = 3
	numAttrs      AttrKey = 4

	// AttrUnknown is returned by AttrKeyForTag when no promoted tag matches.
	// Its value is intentionally out of range for vals[] so misuse panics immediately.
	AttrUnknown AttrKey = 0xFF
)

// Compile-time guard: the numeric values of AttrKey constants are load-bearing —
// they index both vals[] and setMask bit positions. If any value drifts (e.g. via
// iota + reorder), the expression below produces a compile error.
var (
	_ = [1]byte{}[AttrEnv]         // AttrEnv must be 0
	_ = [1]byte{}[AttrVersion-1]   // AttrVersion must be 1
	_ = [1]byte{}[AttrComponent-2] // AttrComponent must be 2
	_ = [1]byte{}[AttrSpanKind-3]  // AttrSpanKind must be 3
)

// SpanAttributes holds the V1-protocol promoted span fields.
// Zero value = all fields absent.
// Set(key, "") is distinct from never-Set: the bit is set, the string is "".
//
// Layout: 1-byte setMask + 1-byte shared + 6B padding + [4]string (64B) = 72 bytes.
//
// When shared is true, the instance is owned by the tracer and must not be
// mutated. Callers must Clone before writing (copy-on-write).
type SpanAttributes struct {
	setMask uint8
	shared  bool
	vals    [numAttrs]string
}

// All read methods are nil-safe so callers holding a *SpanAttributes don't
// need nil guards.

func (a *SpanAttributes) Set(key AttrKey, v string) {
	a.vals[key] = v
	a.setMask |= 1 << key
}

// Unset clears the attribute for key, making it absent (as if never set). nil-safe.
func (a *SpanAttributes) Unset(key AttrKey) {
	if a == nil {
		return
	}
	a.vals[key] = ""
	a.setMask &^= 1 << key
}

func (a *SpanAttributes) Val(key AttrKey) string {
	if a == nil {
		return ""
	}
	return a.vals[key]
}

func (a *SpanAttributes) Has(key AttrKey) bool {
	return a != nil && a.setMask>>key&1 != 0
}

func (a *SpanAttributes) Get(key AttrKey) (string, bool) {
	return a.Val(key), a.Has(key)
}

// Count returns the number of promoted fields that have been set.
func (a *SpanAttributes) Count() int {
	if a == nil {
		return 0
	}
	return bits.OnesCount8(a.setMask)
}

// MarkShared marks this instance as shared (read-only). Clone before mutating.
func (a *SpanAttributes) MarkShared() { a.shared = true }

// IsShared reports whether this is a shared instance requiring COW.
func (a *SpanAttributes) IsShared() bool { return a != nil && a.shared }

// Reset clears all set attributes, returning the instance to its zero state.
// It is nil-safe and does not free the underlying memory, making it suitable
// for reuse (e.g. in a decode loop that reuses Span objects).
func (a *SpanAttributes) Reset() {
	if a == nil {
		return
	}
	*a = SpanAttributes{}
}

// Clone returns a mutable (non-shared) shallow copy.
func (a *SpanAttributes) Clone() *SpanAttributes {
	if a == nil {
		return &SpanAttributes{}
	}
	cp := *a
	cp.shared = false
	return &cp
}

// AttrDef maps an AttrKey to its canonical tag name.
type AttrDef struct {
	Key  AttrKey
	Name string
}

// Defs enumerates all promoted attribute definitions.
var Defs = [numAttrs]AttrDef{
	{AttrEnv, "env"},
	{AttrVersion, "version"},
	{AttrComponent, "component"},
	{AttrSpanKind, "span.kind"},
}

// All returns an iterator over the set attributes (name, value) pairs.
func (a *SpanAttributes) All() iter.Seq2[string, string] {
	return func(yield func(string, string) bool) {
		if a == nil {
			return
		}
		for _, d := range Defs {
			if a.Has(d.Key) {
				if !yield(d.Name, a.vals[d.Key]) {
					return
				}
			}
		}
	}
}

// AttrKeyForTag returns the AttrKey for a promoted tag name, if any.
// Returns (AttrUnknown, false) when the tag is not a promoted attribute.
func AttrKeyForTag(tag string) (AttrKey, bool) {
	if !IsPromotedKeyLen(len(tag)) {
		return AttrUnknown, false
	}
	switch tag {
	case "env":
		return AttrEnv, true
	case "version":
		return AttrVersion, true
	case "component":
		return AttrComponent, true
	case "span.kind":
		return AttrSpanKind, true
	}
	return AttrUnknown, false
}
