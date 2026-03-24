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

// SpanMeta holds all string meta-tags for a span.
//
// Promoted attributes (env, version, component, span.kind) live in attrs and
// are excluded from the tag store. The msgp codec merges both sources
// transparently so the wire format is unchanged.
//
// Hot paths (setMetaInit, setMetricLocked) use SetMap/DeleteMap for direct
// tag-store access without promoted-key overhead. Callers that may write
// promoted keys use Set/Delete or setMetaTagLocked which route to attrs via
// copy-on-write. Promoted keys never appear in the tag store until Inline() is
// called at span finish.
type SpanMeta struct {
	tags    TagStore[string]
	attrs   *SpanAttributes
	inlined bool
}

// NewSpanMeta returns a SpanMeta initialized with shared attrs (used during span creation).
func NewSpanMeta(attrs *SpanAttributes) SpanMeta {
	return SpanMeta{attrs: attrs}
}

// NewSpanMetaFromMap returns a SpanMeta pre-loaded with a flat map. Intended for test helpers.
func NewSpanMetaFromMap(m map[string]string) SpanMeta {
	return SpanMeta{tags: TagStore[string]{m: m}}
}

// IsZero reports whether the SpanMeta contains no entries (map or promoted).
// The msgp generator emits z.meta.IsZero() for the omitempty check.
func (sm SpanMeta) IsZero() bool {
	return sm.tags.Len() == 0 && sm.attrs.Count() == 0
}

// ReplaceSharedAttrs replaces the current attrs pointer with next if it
// currently equals prev. Used by the tracer to upgrade a newly-created span
// from the base shared attrs to the main-service shared attrs.
func (sm *SpanMeta) ReplaceSharedAttrs(prev, next *SpanAttributes) {
	if sm.attrs == prev {
		sm.attrs = next
	}
}

// Normalize resets the tag store when empty and nils out attrs when empty,
// so that a zero-length SpanMeta compares equal to a freshly-zeroed one.
// Intended for test helpers.
func (sm *SpanMeta) Normalize() {
	if sm.tags.Len() == 0 {
		sm.tags.Reset()
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
	return sm.tags.Get(key)
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

// Attr returns a promoted attribute value by AttrKey. O(1) array index + bitmask.
func (sm SpanMeta) Attr(key AttrKey) (string, bool) {
	return sm.attrs.Get(key)
}

// Range calls fn for each entry in the tag store. After Inline(), this includes
// promoted attrs. fn must be a named function, not a closure, to avoid
// heap-allocating the function value. Iteration stops if fn returns false.
func (sm SpanMeta) Range(fn func(k, v string) bool) {
	sm.tags.Range(fn)
}

// ---------------------------------------------------------------------------
// Write methods — fast (map-only) and full (promoted-aware) variants.
// ---------------------------------------------------------------------------

// SetMap writes key=value directly to the tag store without checking for
// promoted attributes. This method is designed to be inlinable so that
// setMetaInit remains inlinable.
func (sm *SpanMeta) SetMap(key, value string) {
	sm.tags.Set(key, value)
}

// DeleteMap removes key from the tag store only.
// Like SetMap, this bypasses promoted-key routing for performance.
func (sm *SpanMeta) DeleteMap(key string) {
	sm.tags.Delete(key)
}

// Set sets key→value, routing promoted keys to attrs (with copy-on-write)
// and others to the tag store.
// +checklocksignore — called both at init time (no lock) and under lock.
func (sm *SpanMeta) Set(key, value string) {
	if IsPromotedKeyLen(len(key)) && sm.setPromoted(key, value) {
		return
	}
	sm.tags.Set(key, value)
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
	if sm.attrs != nil && sm.attrs.Has(ak) && sm.attrs.Val(ak) == value {
		return true // no-op: key is present and value already matches
	}
	sm.ensureAttrsLocal()
	sm.attrs.Set(ak, value)
	return true
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

// Delete removes key from both the tag store and (for promoted keys) attrs.
// +checklocksignore — called both at init time (no lock) and under lock.
func (sm *SpanMeta) Delete(key string) {
	sm.tags.Delete(key)
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

// attrsNotInStore counts promoted attrs set in attrs but not yet in the tag
// store. Returns 0 after Inline() has been called. Relies on the invariant
// that promoted keys are never written to the store directly (only via Inline),
// so the count equals attrs.Count() until Inline() sets the inlined flag.
func (sm SpanMeta) attrsNotInStore() int {
	if sm.inlined || sm.attrs == nil {
		return 0
	}
	return sm.attrs.Count()
}

// Inline copies all promoted attr values into the tag store. Call once at span
// finish, after all mutations and before tracer.submit(). Does not clear attrs —
// attrs remains the authoritative fast source for promoted-key reads via Get().
func (sm *SpanMeta) Inline() {
	if sm.attrs == nil || sm.attrs.Count() == 0 {
		return
	}
	for _, d := range Defs {
		if sm.attrs.Has(d.Key) {
			sm.tags.Set(d.Name, sm.attrs.vals[d.Key])
		}
	}
	sm.inlined = true
}

// Count returns the total number of distinct entries (tag store + promoted attrs
// not yet inlined). After Inline(), all promoted attrs are in the store.
func (sm SpanMeta) Count() int {
	return sm.tags.Len() + sm.attrsNotInStore()
}

// AttrCount returns the number of promoted attrs currently set.
func (sm SpanMeta) AttrCount() int {
	return sm.attrs.Count()
}

// Merge returns a new map containing all tag store entries plus any promoted
// attrs not yet inlined. Always allocates — never returns the backing store map
// directly, avoiding races when the result is placed into a pooled struct.
func (sm SpanMeta) Merge() map[string]string {
	n := sm.tags.Len() + sm.attrsNotInStore()
	if n == 0 {
		return nil
	}
	m := make(map[string]string, n)
	sm.tags.Range(func(k, v string) bool {
		m[k] = v
		return true
	})
	if !sm.inlined && sm.attrs != nil {
		for _, d := range Defs {
			if sm.attrs.Has(d.Key) {
				m[d.Name] = sm.attrs.vals[d.Key]
			}
		}
	}
	return m
}

// All returns an iterator over all entries. Tag-store entries are yielded first
// (in unspecified order), followed by promoted attributes in definition order
// (env, version, component, span.kind) that are not yet inlined.
// Returning false from yield stops iteration.
func (sm SpanMeta) All() iter.Seq2[string, string] {
	return func(yield func(string, string) bool) {
		cont := true
		sm.tags.Range(func(k, v string) bool {
			if !yield(k, v) {
				cont = false
				return false
			}
			return true
		})
		if !cont || sm.inlined || sm.attrs == nil {
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
		fmt.Fprintf(&b, "%s:%s", k, v)
	}
	b.WriteByte(']')
	return b.String()
}

// ---------------------------------------------------------------------------
// msgp codec
// ---------------------------------------------------------------------------

// EncodeMsg writes the combined map header (tag store entries + promoted attrs
// not yet inlined), then emits all entries. After Inline(), the attrs loop is skipped.
func (sm *SpanMeta) EncodeMsg(en *msgp.Writer) error {
	n := sm.attrsNotInStore()
	if err := en.WriteMapHeader(uint32(sm.tags.Len() + n)); err != nil {
		return msgp.WrapError(err, "Meta")
	}
	// Iterate the backing map directly (same package) to avoid closure overhead.
	for k, v := range sm.tags.m {
		if err := en.WriteString(k); err != nil {
			return msgp.WrapError(err, "Meta")
		}
		if err := en.WriteString(v); err != nil {
			return msgp.WrapError(err, "Meta", k)
		}
	}
	if sm.attrs == nil || n == 0 {
		return nil
	}
	for _, d := range Defs {
		v, ok := sm.attrs.Get(d.Key)
		if !ok {
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

// DecodeMsg reads a msgp map into the tag store. All keys — including promoted
// ones — go into the tag store so that no SpanAttributes allocation is needed
// on the decode path. attrs is only populated on the encode (span-creation) path.
func (sm *SpanMeta) DecodeMsg(dc *msgp.Reader) error {
	header, err := dc.ReadMapHeader()
	if err != nil {
		return msgp.WrapError(err, "Meta")
	}
	sm.tags.clearOrReserve(int(header))
	for range header {
		key, err := dc.ReadString()
		if err != nil {
			return msgp.WrapError(err, "Meta")
		}
		val, err := dc.ReadString()
		if err != nil {
			return msgp.WrapError(err, "Meta", key)
		}
		sm.tags.Set(key, val)
	}
	return nil
}

// Msgsize returns an upper bound estimate of the serialized size.
func (sm *SpanMeta) Msgsize() int {
	size := msgp.MapHeaderSize
	for k, v := range sm.tags.m {
		size += msgp.StringPrefixSize + len(k) + msgp.StringPrefixSize + len(v)
	}
	if n := sm.attrsNotInStore(); sm.attrs != nil && n > 0 {
		for _, d := range Defs {
			if v, ok := sm.attrs.Get(d.Key); ok {
				size += msgp.StringPrefixSize + len(d.Name) + msgp.StringPrefixSize + len(v)
			}
		}
	}
	return size
}
