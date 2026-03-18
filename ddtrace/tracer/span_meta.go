// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2025 Datadog, Inc.

package tracer

import (
	"fmt"
	"iter"
	"strings"

	"github.com/tinylib/msgp/msgp"

	tinternal "github.com/DataDog/dd-trace-go/v2/ddtrace/tracer/internal"
)

var (
	_ msgp.Encodable = (*spanMeta)(nil)
	_ msgp.Decodable = (*spanMeta)(nil)
	_ msgp.Sizer     = (*spanMeta)(nil)
)

// spanMeta replaces a plain map[string]string for the Span.meta field.
// Promoted attributes (env, version, component, span.kind) live in attrs
// and are excluded from the map m. The msgp codec merges both sources
// transparently so the wire format is unchanged.
type spanMeta struct {
	m     map[string]string
	attrs *tinternal.SpanAttributes
}

// IsZero reports whether the spanMeta contains no entries (map or promoted).
// The msgp generator emits z.meta.IsZero() for the omitempty check.
func (sm spanMeta) IsZero() bool {
	return len(sm.m) == 0 && sm.attrs.Count() == 0
}

// ReplaceSharedAttrs replaces the current attrs pointer with next if it
// currently equals prev. Used by the tracer to upgrade a newly-created span
// from the base shared attrs to the main-service shared attrs.
func (sm *spanMeta) ReplaceSharedAttrs(prev, next *tinternal.SpanAttributes) {
	if sm.attrs == prev {
		sm.attrs = next
	}
}

// Normalize sets m and attrs to nil when they are empty so that a zero-length
// spanMeta compares equal to a freshly-zeroed one. Intended for test helpers.
func (sm *spanMeta) Normalize() {
	if len(sm.m) == 0 {
		sm.m = nil
	}
	if sm.attrs != nil && sm.attrs.Count() == 0 {
		sm.attrs = nil
	}
}

// Get returns the value for key, checking attrs for promoted keys and the flat map for others.
func (sm spanMeta) Get(key string) (string, bool) {
	if ak, ok := tinternal.AttrKeyForTag(key); ok {
		return sm.attrs.Get(ak)
	}
	v, ok := sm.m[key]
	return v, ok
}

// Has reports whether key is present. Promoted keys check attrs; others check the flat map.
func (sm spanMeta) Has(key string) bool {
	if ak, ok := tinternal.AttrKeyForTag(key); ok {
		_, ok := sm.attrs.Get(ak)
		return ok
	}
	_, ok := sm.m[key]
	return ok
}

// Set sets key→value, routing promoted keys to attrs (with copy-on-write) and others to the flat map.
// +checklocksignore — called both at init time (no lock) and under lock.
func (sm *spanMeta) Set(key, value string) {
	if ak, ok := tinternal.AttrKeyForTag(key); ok {
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
func (sm *spanMeta) ensureAttrsLocal() {
	if sm.attrs == nil {
		sm.attrs = new(tinternal.SpanAttributes)
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
func (sm *spanMeta) setAttrCOWSlow(key tinternal.AttrKey, v string) {
	sm.ensureAttrsLocal()
	sm.attrs.Set(key, v)
}

// Delete removes key. For promoted keys, clears the attribute (with copy-on-write); for others,
// removes from the flat map. No-op if the key is absent.
// +checklocksignore — called both at init time (no lock) and under lock.
func (sm *spanMeta) Delete(key string) {
	if ak, ok := tinternal.AttrKeyForTag(key); ok {
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
func (sm spanMeta) Count() int {
	return len(sm.m) + sm.attrs.Count()
}

// All returns an iterator over all entries. Flat-map entries are yielded first
// (in unspecified order), followed by promoted attributes in definition order
// (env, version, component, span.kind). Returning false from yield stops iteration.
func (sm spanMeta) All() iter.Seq2[string, string] {
	return func(yield func(string, string) bool) {
		for k, v := range sm.m {
			if !yield(k, v) {
				return
			}
		}
		if sm.attrs != nil {
			for _, d := range tinternal.Defs {
				if v, ok := sm.attrs.Get(d.Key); ok {
					if !yield(d.Name, v) {
						return
					}
				}
			}
		}
	}
}

// String returns a merged map representation (m + promoted attrs) for debug logging.
func (sm spanMeta) String() string {
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
func (sm *spanMeta) EncodeMsg(en *msgp.Writer) error {
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
		for _, d := range tinternal.Defs {
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
func (sm *spanMeta) DecodeMsg(dc *msgp.Reader) error {
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
func (sm *spanMeta) Msgsize() int {
	size := msgp.MapHeaderSize
	for k, v := range sm.m {
		size += msgp.StringPrefixSize + len(k) + msgp.StringPrefixSize + len(v)
	}
	if sm.attrs != nil {
		for _, d := range tinternal.Defs {
			if v, ok := sm.attrs.Get(d.Key); ok {
				size += msgp.StringPrefixSize + len(d.Name) + msgp.StringPrefixSize + len(v)
			}
		}
	}
	return size
}
