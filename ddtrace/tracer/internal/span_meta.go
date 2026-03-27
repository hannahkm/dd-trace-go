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

	"github.com/DataDog/dd-trace-go/v2/internal/locking"
)

// metaMapHint is the initial capacity for the flat map m.
// A typical span carries "language" plus a handful of internal tags
// (_dd.base_service, runtime-id, etc.). 5 accommodates these without
// a rehash and matches the pre-refactor initMeta allocation profile.
const (
	// expectedEntries should be the count of tags known at construction time.
	expectedEntries = 5
	// loadFactor of 4/3 (≈ inverse of the standard 0.75 load factor) provides
	// ~33% slack so small overestimates don't trigger an immediate rehash.
	loadFactor  = 4 / 3
	metaMapHint = expectedEntries * loadFactor
)

var (
	_ msgp.Encodable = (*SpanMeta)(nil)
	_ msgp.Decodable = (*SpanMeta)(nil)
	_ msgp.Sizer     = (*SpanMeta)(nil)
)

// SpanMeta replaces a plain map[string]string for the Span.meta field.
// Promoted attributes (env, version, component, span.kind, language) live in
// attrs and are excluded from the map m. The msgp codec merges both sources
// transparently so the wire format is unchanged.
//
// Set routes promoted keys to attrs (with copy-on-write) and others to the
// flat map. Promoted keys never appear in sm.m until Map() is called.
//
// Map() inlines promoted attrs into sm.m under mu, then returns sm.m directly
// (zero allocation on the hot stats path). EncodeMsg, Msgsize, Range,
// SerializableCount, and IsZero also acquire mu so they see a consistent view
// of sm.m during concurrent serialization.
type SpanMeta struct {
	m       map[string]string
	attrs   *SpanAttributes
	mu      locking.Mutex
	inlined bool
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
func (sm *SpanMeta) IsZero() bool {
	sm.mu.Lock()
	defer sm.mu.Unlock()
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
func (sm *SpanMeta) Get(key string) (string, bool) {
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
func (sm *SpanMeta) getPromoted(key string) (string, bool, bool) {
	ak, ok := AttrKeyForTag(key)
	if !ok {
		return "", false, false
	}
	v, found := sm.attrs.Get(ak)
	return v, found, true
}

// Has reports whether key is present.
func (sm *SpanMeta) Has(key string) bool {
	_, ok := sm.Get(key)
	return ok
}

// Attr returns a promoted attribute value by AttrKey. O(1) array index + bitmask.
func (sm *SpanMeta) Attr(key AttrKey) (string, bool) {
	return sm.attrs.Get(key)
}

// Env returns the value of the "env" promoted attribute.
func (sm *SpanMeta) Env() (string, bool) { return sm.attrs.Get(AttrEnv) }

// Version returns the value of the "version" promoted attribute.
func (sm *SpanMeta) Version() (string, bool) { return sm.attrs.Get(AttrVersion) }

// Component returns the value of the "component" promoted attribute.
func (sm *SpanMeta) Component() (string, bool) { return sm.attrs.Get(AttrComponent) }

// SpanKind returns the value of the "span.kind" promoted attribute.
func (sm *SpanMeta) SpanKind() (string, bool) { return sm.attrs.Get(AttrSpanKind) }

// Language returns the value of the "language" promoted attribute.
func (sm *SpanMeta) Language() (string, bool) { return sm.attrs.Get(AttrLanguage) }

// Range calls fn for each entry in the flat map (sm.m), skipping any promoted
// keys that were inlined into sm.m by a previous Map() call. Promoted attrs
// live in sm.attrs and are not yielded here (or in v1 they are encoded as
// dedicated fields 13-16). Iteration stops if fn returns false.
func (sm *SpanMeta) Range(fn func(k, v string) bool) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	for k, v := range sm.m {
		if sm.inlined && isPromotedKey(k) {
			continue
		}
		if !fn(k, v) {
			return
		}
	}
}

// isPromotedKey reports whether k is an exact promoted attribute name.
// Only called on the hot path when inlined=true, so the extra check is rare.
func isPromotedKey(k string) bool {
	_, ok := AttrKeyForTag(k)
	return ok
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

// setPromoted is the slow path for Set when the key might be a promoted
// attribute. Returns true if the key was handled (set or no-op).
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
//
// The length switch is intentionally duplicated from IsPromotedKeyLen rather
// than calling it. Inlining IsPromotedKeyLen (cost 11) into Delete raises
// Delete's budget from 73 to 81, crossing the 80-unit limit and preventing
// callers from inlining Delete. The direct switch keeps Delete at cost 73.
func (sm *SpanMeta) Delete(key string) {
	switch len(key) {
	case 3, 7, 8, 9:
		sm.deleteSlow(key)
	default:
		delete(sm.m, key)
	}
}

// deleteSlow handles the promoted-key path for Delete.
func (sm *SpanMeta) deleteSlow(key string) {
	delete(sm.m, key)
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
// Promoted keys: "env"(3), "version"(7), "language"(8), "component"(9), "span.kind"(9).
// This must stay in sync with the Defs table in span_attributes.go; the init
// check below enforces this at program start.
func IsPromotedKeyLen(n int) bool {
	switch n {
	case 3, 7, 8, 9:
		return true
	}
	return false
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
func (sm *SpanMeta) Count() int {
	return len(sm.m) + sm.attrs.Count()
}

// AttrCount returns the number of promoted attrs currently set.
func (sm *SpanMeta) AttrCount() int {
	return sm.attrs.Count()
}

// SerializableCount returns the number of flat-map entries that appear in the
// serialized attributes array. Promoted attrs are encoded as dedicated fields
// in the v1 protocol and must not be double-counted. When Map() has been called,
// promoted keys are also in sm.m, so they are subtracted from the count.
func (sm *SpanMeta) SerializableCount() int {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if sm.inlined {
		return len(sm.m) - sm.attrs.Count()
	}
	return len(sm.m)
}

// Map inlines all promoted attrs into sm.m on first call, then returns sm.m
// directly — zero allocation on subsequent calls. Both Map and the serialization
// methods (EncodeMsg, Msgsize, Range, SerializableCount, IsZero) acquire mu so
// they observe a consistent view of sm.m during concurrent access.
func (sm *SpanMeta) Map() map[string]string {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if !sm.inlined {
		if sm.attrs != nil {
			if n := sm.attrs.Count(); n > 0 {
				if sm.m == nil {
					sm.m = make(map[string]string, n)
				}
				for _, d := range Defs {
					if sm.attrs.Has(d.Key) {
						sm.m[d.Name] = sm.attrs.vals[d.Key]
					}
				}
			}
		}
		sm.inlined = true
	}
	return sm.m
}

// All returns an iterator over all entries. Flat-map entries are yielded first
// (in unspecified order), followed by promoted attributes in definition order
// (env, version, component, span.kind). Returning false from yield stops iteration.
func (sm *SpanMeta) All() iter.Seq2[string, string] {
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
func (sm *SpanMeta) String() string {
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

// EncodeMsg writes the map header and entries. When Map() has already been
// called (inlined=true), sm.m contains all entries and attrs is skipped to
// avoid double-encoding. Otherwise the flat map and promoted attrs are
// combined as normal.
func (sm *SpanMeta) EncodeMsg(en *msgp.Writer) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if sm.inlined {
		if err := en.WriteMapHeader(uint32(len(sm.m))); err != nil {
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
		return nil
	}
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

// Msgsize returns an upper bound estimate of the serialized size. When Map()
// has already been called (inlined=true), sm.m contains all entries so attrs
// is not separately sized to avoid double-counting.
func (sm *SpanMeta) Msgsize() int {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	size := msgp.MapHeaderSize
	for k, v := range sm.m {
		size += msgp.StringPrefixSize + len(k) + msgp.StringPrefixSize + len(v)
	}
	if sm.inlined {
		return size
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
