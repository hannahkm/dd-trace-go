// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2025 Datadog, Inc.

package internal

import (
	"fmt"
	"iter"
	"strings"

	"github.com/tinylib/msgp/msgp"
)

var (
	_ msgp.Encodable = (*SpanMeta)(nil)
	_ msgp.Decodable = (*SpanMeta)(nil)
	_ msgp.Sizer     = (*SpanMeta)(nil)
)

// SpanMeta replaces a plain map[string]string for the Span.meta field.
// Promoted attributes (env, version, component, span.kind) live in attrs
// and are excluded from the map m. The msgp codec merges both sources
// transparently so the wire format is unchanged.
type SpanMeta struct {
	m     map[string]string
	attrs *SpanAttributes
}

// NewSpanMeta returns a SpanMeta initialized with shared attrs (used during span creation).
func NewSpanMeta(attrs *SpanAttributes) SpanMeta {
	return SpanMeta{attrs: attrs}
}

// NewSpanMetaFromMap returns a SpanMeta pre-loaded with a flat map. Intended for test helpers.
func NewSpanMetaFromMap(m map[string]string) SpanMeta {
	return SpanMeta{m: m}
}

// IsZero reports whether the SpanMeta contains no entries (map or promoted).
// The msgp generator emits z.meta.IsZero() for the omitempty check.
func (sm SpanMeta) IsZero() bool {
	return len(sm.m) == 0 && sm.attrs.Count() == 0
}

// ReplaceSharedAttrs replaces the current attrs pointer with next if it
// currently equals prev. Used by the tracer to upgrade a newly-created span
// from the base shared attrs to the main-service shared attrs.
func (sm *SpanMeta) ReplaceSharedAttrs(prev, next *SpanAttributes) {
	if sm.attrs == prev {
		sm.attrs = next
	}
}

// Normalize sets m and attrs to nil when they are empty so that a zero-length
// SpanMeta compares equal to a freshly-zeroed one. Intended for test helpers.
func (sm *SpanMeta) Normalize() {
	if len(sm.m) == 0 {
		sm.m = nil
	}
	if sm.attrs != nil && sm.attrs.Count() == 0 {
		sm.attrs = nil
	}
}

// Get returns the value for key, checking attrs for promoted keys and the flat map for others.
func (sm SpanMeta) Get(key string) (string, bool) {
	if ak, ok := AttrKeyForTag(key); ok {
		return sm.attrs.Get(ak)
	}
	v, ok := sm.m[key]
	return v, ok
}

// Has reports whether key is present. Promoted keys check attrs; others check the flat map.
func (sm SpanMeta) Has(key string) bool {
	if ak, ok := AttrKeyForTag(key); ok {
		_, ok := sm.attrs.Get(ak)
		return ok
	}
	_, ok := sm.m[key]
	return ok
}

// Set sets key→value, routing promoted keys to attrs (with copy-on-write) and others to the flat map.
// +checklocksignore — called both at init time (no lock) and under lock.
func (sm *SpanMeta) Set(key, value string) {
	if ak, ok := AttrKeyForTag(key); ok {
		// Check if whether setting key=v on attrs would be a no-op
		// no-op (i.e. the current value is already v, shared or local).
		if sm.attrs != nil && sm.attrs.Val(ak) == value {
			return
		}
		sm.setAttrCOWSlow(ak, value)
		return
	}
	if sm.m == nil {
		sm.m = make(map[string]string, 1)
	}
	sm.m[key] = value
}

// ensureAttrsLocal guarantees attrs is a mutable, span-local instance.
// If attrs is nil a fresh one is allocated; if shared, it is cloned.
func (sm *SpanMeta) ensureAttrsLocal() {
	if sm.attrs == nil {
		sm.attrs = new(SpanAttributes)
		return
	}
	if sm.attrs.IsShared() {
		sm.attrs = sm.attrs.Clone()
	}
}

// setAttrCOWSlow is the slow path for Set when the value is a promoted attr that needs writing.
// Separated with //go:noinline to keep the hot path in Set small.
//
//go:noinline
func (sm *SpanMeta) setAttrCOWSlow(key AttrKey, v string) {
	sm.ensureAttrsLocal()
	sm.attrs.Set(key, v)
}

// Delete removes key. For promoted keys, clears the attribute (with copy-on-write); for others,
// removes from the flat map. No-op if the key is absent.
// +checklocksignore — called both at init time (no lock) and under lock.
func (sm *SpanMeta) Delete(key string) {
	if ak, ok := AttrKeyForTag(key); ok {
		if _, isSet := sm.attrs.Get(ak); !isSet {
			return // already absent, skip unnecessary COW
		}
		sm.ensureAttrsLocal()
		sm.attrs.Unset(ak)
		return
	}
	delete(sm.m, key)
}

// Count returns the total number of entries (flat map + promoted attrs).
func (sm SpanMeta) Count() int {
	return len(sm.m) + sm.attrs.Count()
}

// All returns an iterator over all entries. Flat-map entries are yielded first
// (in unspecified order), followed by promoted attributes in definition order
// (env, version, component, span.kind). Returning false from yield stops iteration.
func (sm SpanMeta) All() iter.Seq2[string, string] {
	return func(yield func(string, string) bool) {
		for k, v := range sm.m {
			if !yield(k, v) {
				return
			}
		}
		if sm.attrs != nil {
			for _, d := range Defs {
				if sm.attrs.setMask>>d.Key&1 != 0 {
					if !yield(d.Name, sm.attrs.vals[d.Key]) {
						return
					}
				}
			}
		}
	}
}

// String returns a merged map representation (m + promoted attrs) for debug logging.
func (sm SpanMeta) String() string {
	var b strings.Builder
	b.WriteString("map[")
	first := true
	for k, v := range sm.All() {
		if !first {
			b.WriteByte(' ')
		}
		first = false
		fmt.Fprintf(&b, "%s:%s", k, v)
	}
	b.WriteByte(']')
	return b.String()
}

// EncodeMsg writes the combined map header (m entries + promoted attrs),
// then emits all map entries followed by promoted attribute entries.
func (sm *SpanMeta) EncodeMsg(en *msgp.Writer) error {
	total := uint32(len(sm.m) + sm.attrs.Count())
	if err := en.WriteMapHeader(total); err != nil {
		return msgp.WrapError(err, "Meta")
	}
	for k, v := range sm.m {
		if err := en.WriteString(k); err != nil {
			return msgp.WrapError(err, "Meta")
		}
		if err := en.WriteString(v); err != nil {
			return msgp.WrapError(err, "Meta", k)
		}
	}
	if sm.attrs != nil {
		for _, d := range Defs {
			if v, ok := sm.attrs.Get(d.Key); ok {
				if err := en.WriteString(d.Name); err != nil {
					return msgp.WrapError(err, "Meta")
				}
				if err := en.WriteString(v); err != nil {
					return msgp.WrapError(err, "Meta", d.Name)
				}
			}
		}
	}
	return nil
}

// DecodeMsg reads a msgp map into m. All keys — including promoted ones — go
// into the flat map so that no SpanAttributes allocation is needed on the
// decode path. attrs is only populated on the encode (span-creation) path.
func (sm *SpanMeta) DecodeMsg(dc *msgp.Reader) error {
	header, err := dc.ReadMapHeader()
	if err != nil {
		return msgp.WrapError(err, "Meta")
	}
	// Reuse sm.m if already allocated; otherwise allocate fresh pre-sized.
	if sm.m != nil {
		clear(sm.m)
	} else {
		sm.m = make(map[string]string, header)
	}
	for range header {
		key, err := dc.ReadString()
		if err != nil {
			return msgp.WrapError(err, "Meta")
		}
		val, err := dc.ReadString()
		if err != nil {
			return msgp.WrapError(err, "Meta", key)
		}
		sm.m[key] = val
	}
	return nil
}

// Msgsize returns an upper bound estimate of the serialized size.
func (sm *SpanMeta) Msgsize() int {
	size := msgp.MapHeaderSize
	for k, v := range sm.m {
		size += msgp.StringPrefixSize + len(k) + msgp.StringPrefixSize + len(v)
	}
	if sm.attrs != nil {
		for _, d := range Defs {
			if v, ok := sm.attrs.Get(d.Key); ok {
				size += msgp.StringPrefixSize + len(d.Name) + msgp.StringPrefixSize + len(v)
			}
		}
	}
	return size
}
