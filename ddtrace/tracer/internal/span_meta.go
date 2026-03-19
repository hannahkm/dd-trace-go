// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2025 Datadog, Inc.

package internal

import (
	"fmt"
	"iter"
	"maps"
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
//
// Hot paths (setMetaInit, setMetricLocked) use SetMap/DeleteMap for direct
// flat-map access without promoted-key overhead. Callers that may write
// promoted keys use Set/Delete or setMetaTagLocked which route to attrs
// via copy-on-write. This ensures promoted keys never appear in m.
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

// Get returns the value for key, checking the flat map then attrs for promoted keys.
func (sm SpanMeta) Get(key string) (string, bool) {
	if v, ok := sm.m[key]; ok {
		return v, ok
	}
	if IsPromotedKeyLen(len(key)) {
		if v, ok, handled := sm.getPromoted(key); handled {
			return v, ok
		}
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

// ---------------------------------------------------------------------------
// Write methods — fast (map-only) and full (promoted-aware) variants.
// ---------------------------------------------------------------------------

// SetMap writes key=value directly to the flat map without checking for
// promoted attributes. It is the caller's responsibility to ensure that
// promoted-key routing is handled externally (or that the encoding's
// dedup logic will cover the overlap). This method is designed to be
// inlinable so that setMetaInit remains inlinable.
func (sm *SpanMeta) SetMap(key, value string) {
	if sm.m == nil {
		sm.m = make(map[string]string, 1)
	}
	sm.m[key] = value
}

// DeleteMap removes key from the flat map only. Safe to call on a nil map.
// Like SetMap, this bypasses promoted-key routing for performance.
func (sm *SpanMeta) DeleteMap(key string) {
	delete(sm.m, key)
}

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

// setPromoted is the slow path for Set when the key might be a promoted attribute.
// Returns true if the key was handled (set or skipped as no-op).
//
//go:noinline
func (sm *SpanMeta) setPromoted(key, value string) bool {
	ak, ok := AttrKeyForTag(key)
	if !ok {
		return false
	}
	if sm.attrs != nil && sm.attrs.Val(ak) == value {
		return true // no-op: value already matches
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
	sm.m = make(map[string]string, 1)
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
// Counting / iteration — promoted keys are in attrs only, never in m.
// ---------------------------------------------------------------------------

// Count returns the total number of entries (flat map + promoted attrs).
func (sm SpanMeta) Count() int {
	return len(sm.m) + sm.attrs.Count()
}

// Merge returns a map[string]string containing all entries (flat map + promoted attrs).
// When no promoted attrs are set, the internal flat map is returned directly without
// allocating — the caller must not mutate it. When promoted attrs are present, a new
// merged map is allocated and returned.
func (sm SpanMeta) Merge() map[string]string {
	if sm.attrs.Count() == 0 {
		return sm.m // nil-safe: callers must handle a nil map
	}
	m := make(map[string]string, len(sm.m)+sm.attrs.Count())
	maps.Copy(m, sm.m)
	for _, d := range Defs {
		if sm.attrs.setMask>>d.Key&1 != 0 {
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

// ---------------------------------------------------------------------------
// msgp codec
// ---------------------------------------------------------------------------

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
