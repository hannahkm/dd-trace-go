// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2025 Datadog, Inc.

package internal

import (
	"iter"
	"strings"

	"github.com/tinylib/msgp/msgp"
)

// metaMapHint is the initial capacity for the flat map m.
// A typical span carries "language" plus a handful of internal tags
// (_dd.base_service, runtime-id, etc.). 5 accommodates these without
// a rehash and matches the pre-refactor initMeta allocation profile.
const metaMapHint = 5

var (
	_ msgp.Encodable = (*SpanMeta)(nil)
	_ msgp.Decodable = (*SpanMeta)(nil)
	_ msgp.Sizer     = (*SpanMeta)(nil)
)

// SpanMeta replaces a plain map[string]string for the Span.meta field.
// Promoted attributes (env, version, component, span.kind) live in attrs
// and are excluded from the map m. The msgp codec merges both sources
// transparently so the wire format is unchanged.
//
// Hot paths (setMetaInit, setMetricLocked) use SetMap/DeleteMap for direct
// tag-store access without promoted-key overhead. Callers that may write
// promoted keys use Set/Delete or setMetaTagLocked which route to attrs via
// copy-on-write. Promoted keys never appear in sm.m.
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

// ---------------------------------------------------------------------------
// Read methods
// ---------------------------------------------------------------------------

// Get returns the value for key. Promoted keys are checked in attrs first
// (fast array+bitmask path), then the flat map. Non-promoted keys go directly
// to the flat map.
func (sm SpanMeta) Get(key string) (string, bool) {
	if IsPromotedKeyLen(len(key)) {
		if v, ok, handled := sm.getPromoted(key); handled {
			return v, ok
		}
	}
	if v, ok := sm.m[key]; ok {
		return v, ok
	}
	return "", false
}

// getPromoted is the slow path for Get when the key might be a promoted attribute.
//
//go:noinline
func (sm SpanMeta) getPromoted(key string) (string, bool, bool) {
	ak, ok := AttrKeyForTag(key)
	if !ok {
		return "", false, false
	}
	v, found := sm.attrs.Get(ak)
	return v, found, true
}

// Has reports whether key is present.
func (sm SpanMeta) Has(key string) bool {
	_, ok := sm.Get(key)
	return ok
}

// Env returns the value of the "env" promoted attribute.
func (sm SpanMeta) Env() (string, bool) { return sm.attrs.Get(AttrEnv) }

// Version returns the value of the "version" promoted attribute.
func (sm SpanMeta) Version() (string, bool) { return sm.attrs.Get(AttrVersion) }

// Component returns the value of the "component" promoted attribute.
func (sm SpanMeta) Component() (string, bool) { return sm.attrs.Get(AttrComponent) }

// SpanKind returns the value of the "span.kind" promoted attribute.
func (sm SpanMeta) SpanKind() (string, bool) { return sm.attrs.Get(AttrSpanKind) }

// SetIfPromoted stores key→value in the promoted-attribute store if key is a
// promoted attribute name (env, version, component, span.kind). Returns true
// when the key was handled; callers can skip their normal tag-store path.
func (sm *SpanMeta) SetIfPromoted(key, value string) bool {
	if !IsPromotedKeyLen(len(key)) {
		return false
	}
	return sm.setPromoted(key, value)
}

// Range calls fn for each entry in the flat map (sm.m).
// Promoted attrs live in sm.attrs and are not yielded here.
// Iteration stops if fn returns false.
func (sm SpanMeta) Range(fn func(k, v string) bool) {
	for k, v := range sm.m {
		if !fn(k, v) {
			return
		}
	}
}

// ---------------------------------------------------------------------------
// Write methods
// ---------------------------------------------------------------------------

// Set sets key→value, routing promoted keys to attrs (with copy-on-write)
// and others to the flat map.
// +checklocksignore — called both at init time (no lock) and under lock.
func (sm *SpanMeta) Set(key, value string) {
	if IsPromotedKeyLen(len(key)) && sm.setPromoted(key, value) {
		return
	}
	if sm.m == nil {
		sm.initMap(key, value)
		return
	}
	sm.m[key] = value
}

// setPromoted is the slow path for Set and SetIfPromoted when the key might be
// a promoted attribute. Returns true if the key was handled (set or no-op).
//
//go:noinline
func (sm *SpanMeta) setPromoted(key, value string) bool {
	ak, ok := AttrKeyForTag(key)
	if !ok {
		return false
	}
	if sm.attrs != nil && sm.attrs.Has(ak) && sm.attrs.Val(ak) == value {
		return true // no-op: key is present and value already matches
	}
	sm.ensureAttrsLocal()
	sm.attrs.Set(ak, value)
	return true
}

// initMap allocates the flat map and inserts the first entry.
// Separated to keep Set's fast path (map already exists) small and inlinable.
//
//go:noinline
func (sm *SpanMeta) initMap(key, value string) {
	sm.m = make(map[string]string, metaMapHint)
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

// Delete removes key from both the flat map and (for promoted keys) attrs.
// +checklocksignore — called both at init time (no lock) and under lock.
func (sm *SpanMeta) Delete(key string) {
	delete(sm.m, key)
	if IsPromotedKeyLen(len(key)) {
		sm.deleteFromAttrs(key)
	}
}

// deleteFromAttrs handles the promoted-key side of Delete.
//
//go:noinline
func (sm *SpanMeta) deleteFromAttrs(key string) {
	ak, ok := AttrKeyForTag(key)
	if !ok {
		return
	}
	if _, isSet := sm.attrs.Get(ak); !isSet {
		return
	}
	sm.ensureAttrsLocal()
	sm.attrs.Unset(ak)
}

// IsPromotedKeyLen reports whether n matches the length of any promoted attribute name.
// Promoted keys: "env"(3), "version"(7), "component"(9), "span.kind"(9).
// This must stay in sync with the Defs table in span_attributes.go; the init
// check below enforces this at program start.
func IsPromotedKeyLen(n int) bool {
	return n == 3 || n == 7 || n == 9
}

func init() {
	for _, d := range Defs {
		if !IsPromotedKeyLen(len(d.Name)) {
			panic("IsPromotedKeyLen out of sync with Defs: missing length " + d.Name)
		}
	}
}

// ---------------------------------------------------------------------------
// Counting / iteration
// ---------------------------------------------------------------------------

// Count returns the total number of distinct entries (flat map + promoted attrs).
func (sm SpanMeta) Count() int {
	return len(sm.m) + sm.attrs.Count()
}

// AttrCount returns the number of promoted attrs currently set.
func (sm SpanMeta) AttrCount() int {
	return sm.attrs.Count()
}

// SerializableCount returns the number of flat-map entries that appear in the
// serialized attributes array. Promoted attrs are encoded as dedicated fields
// in the v1 protocol and must not be double-counted.
func (sm SpanMeta) SerializableCount() int {
	return len(sm.m)
}

// Merge returns a new map containing all flat-map entries plus all promoted
// attrs. Always allocates — never returns sm.m directly, avoiding races when
// the result is placed into a pooled struct.
func (sm SpanMeta) Merge() map[string]string {
	n := len(sm.m) + sm.attrs.Count()
	if n == 0 {
		return nil
	}
	m := make(map[string]string, n)
	for k, v := range sm.m {
		m[k] = v
	}
	for _, d := range Defs {
		if sm.attrs.Has(d.Key) {
			m[d.Name] = sm.attrs.vals[d.Key]
		}
	}
	return m
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
		if sm.attrs == nil {
			return
		}
		for _, d := range Defs {
			if sm.attrs.Has(d.Key) {
				if !yield(d.Name, sm.attrs.vals[d.Key]) {
					return
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
		b.WriteString(k)
		b.WriteByte(':')
		b.WriteString(v)
	}
	b.WriteByte(']')
	return b.String()
}

// ---------------------------------------------------------------------------
// msgp codec
// ---------------------------------------------------------------------------

// EncodeMsg writes the map header (flat map entries + promoted attrs),
// then emits all flat map entries followed by all set promoted attrs.
func (sm *SpanMeta) EncodeMsg(en *msgp.Writer) error {
	n := sm.attrs.Count()
	if err := en.WriteMapHeader(uint32(len(sm.m) + n)); err != nil {
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
	if n == 0 {
		return nil
	}
	var (
		v  string
		ok bool
	)
	for _, d := range Defs {
		if v, ok = sm.attrs.Get(d.Key); !ok {
			continue
		}
		if err := en.WriteString(d.Name); err != nil {
			return msgp.WrapError(err, "Meta")
		}
		if err := en.WriteString(v); err != nil {
			return msgp.WrapError(err, "Meta", d.Name)
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
	if n := sm.attrs.Count(); n > 0 {
		for _, d := range Defs {
			if v, ok := sm.attrs.Get(d.Key); ok {
				size += msgp.StringPrefixSize + len(d.Name) + msgp.StringPrefixSize + len(v)
			}
		}
	}
	return size
}
