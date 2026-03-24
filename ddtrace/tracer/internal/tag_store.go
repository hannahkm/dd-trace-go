// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2025 Datadog, Inc.

package internal

const (
	// tagStoreInline is the number of key/value pairs held in the inline
	// small-vector before the TagStore pivots to a backing map.
	tagStoreInline = 0

	// tagStoreDefaultCap is the initial capacity of the map allocated on pivot.
	tagStoreDefaultCap = 16
)

// TagValue constrains the value type accepted by TagStore.
type TagValue interface {
	string | float64
}

// tagKV is one inline entry.
type tagKV[V TagValue] struct {
	k string
	v V
}

// TagStore is a write-optimised key/value store for string keys.
//
// When [tagStoreInline] > 0, the store starts in small-vector mode: up to
// tagStoreInline entries are held in an inline array, avoiding any heap
// allocation. When the array is full the store pivots once to a backing map.
// When tagStoreInline == 0 the store goes directly to a backing map on first
// write.
//
// Zero value is valid.
type TagStore[V TagValue] struct {
	kvs [tagStoreInline]tagKV[V]
	n   uint8        // number of valid inline entries
	m   map[string]V // non-nil once pivoted or reserved
}

// Set stores key→val. In inline mode it performs a linear-scan upsert; in map
// mode it writes directly to the backing map.
func (s *TagStore[V]) Set(k string, v V) {
	if s.m != nil {
		s.m[k] = v
		return
	}
	for i := range s.n {
		if s.kvs[i].k == k {
			s.kvs[i].v = v
			return
		}
	}
	if int(s.n) < tagStoreInline {
		s.kvs[s.n] = tagKV[V]{k, v}
		s.n++
		return
	}
	s.pivot()
	s.m[k] = v
}

// Get returns the value for key and whether it was found.
func (s *TagStore[V]) Get(k string) (V, bool) {
	if s.m != nil {
		v, ok := s.m[k]
		return v, ok
	}
	for i := range s.n {
		if s.kvs[i].k == k {
			return s.kvs[i].v, true
		}
	}
	var zero V
	return zero, false
}

// Delete removes key if present. In inline mode it swaps the removed entry
// with the last entry to avoid shifting, preserving O(1) deletion.
func (s *TagStore[V]) Delete(k string) {
	if s.m != nil {
		delete(s.m, k)
		return
	}
	for i := range s.n {
		if s.kvs[i].k == k {
			s.n--
			s.kvs[i] = s.kvs[s.n]
			s.kvs[s.n] = tagKV[V]{}
			return
		}
	}
}

// Len returns the number of entries.
func (s *TagStore[V]) Len() int {
	if s.m != nil {
		return len(s.m)
	}
	return int(s.n)
}

// Range calls fn for every entry. Iteration stops when fn returns false.
func (s *TagStore[V]) Range(fn func(k string, v V) bool) {
	if s.m != nil {
		for k, v := range s.m {
			if !fn(k, v) {
				return
			}
		}
		return
	}
	for i := range s.n {
		if !fn(s.kvs[i].k, s.kvs[i].v) {
			return
		}
	}
}

// Reserve ensures capacity for at least n entries. If n exceeds the inline
// capacity the store pivots immediately to a map of size n, bypassing the
// small-vector path. Safe to call on an already-pivoted store.
func (s *TagStore[V]) Reserve(n int) {
	if s.m != nil || n <= tagStoreInline {
		return
	}
	s.pivotWithCap(n)
}

// Reset clears all entries and returns the store to inline mode.
// The backing map (if any) is released for GC.
func (s *TagStore[V]) Reset() {
	s.m = nil
	for i := range s.n {
		s.kvs[i] = tagKV[V]{}
	}
	s.n = 0
}

// clearOrReserve prepares the store for reuse. If a backing map already exists
// it is cleared in place (preserving the allocation); otherwise a new map of
// size n is allocated. Used by DecodeMsg to avoid allocating on repeated decode.
func (s *TagStore[V]) clearOrReserve(n int) {
	s.n = 0
	if s.m != nil {
		clear(s.m)
		return
	}
	s.m = make(map[string]V, n)
}

// pivot allocates the backing map with the default capacity and copies all
// inline entries into it. Inline entries are zeroed after the copy so the
// store has a consistent zero-valued inline array in map mode (required for
// equality comparisons between runtime spans and msgpack-decoded spans).
func (s *TagStore[V]) pivot() {
	s.pivotWithCap(tagStoreDefaultCap)
}

// pivotWithCap is like pivot but uses the provided capacity.
func (s *TagStore[V]) pivotWithCap(n int) {
	s.m = make(map[string]V, n)
	for i := range s.n {
		s.m[s.kvs[i].k] = s.kvs[i].v
		s.kvs[i] = tagKV[V]{}
	}
	s.n = 0
}
