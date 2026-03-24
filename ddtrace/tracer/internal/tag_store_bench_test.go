// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2025 Datadog, Inc.

package internal

import (
	"fmt"
	"math"
	"testing"
)

// productionTagWorkload returns a pre-computed slice of tag counts whose
// distribution matches the one observed in the Datadog production intake.
// The caller should cycle through the slice modulo its length.
//
//	probs  = {0.01, 0.09, 0.40, 0.25, 0.15, 0.05, 0.04, 0.01}
//	counts = {8.75, 14.59, 22.8, 31.2, 39.1, 43.5, 54.3, 70.0}
func productionTagWorkload() []int {
	probs := []float64{0.01, 0.09, 0.4, 0.25, 0.15, 0.05, 0.04, 0.01}
	means := []float64{8.75, 14.59, 22.8, 31.2, 39.1, 43.5, 54.3, 70.0}
	const size = 1000
	workload := make([]int, 0, size)
	for i, p := range probs {
		n := int(math.Round(p * size))
		c := int(math.Round(means[i]))
		for range n {
			workload = append(workload, c)
		}
	}
	return workload
}

// tagPairs returns pre-allocated key/value pairs for up to maxTags tags.
// Keys are realistic-length strings; values are short strings.
func tagPairs(maxTags int) (keys, vals []string) {
	keys = make([]string, maxTags)
	vals = make([]string, maxTags)
	for i := range keys {
		keys[i] = fmt.Sprintf("user.tag.key.%02d", i)
		vals[i] = fmt.Sprintf("v%04d", i)
	}
	return
}

// BenchmarkTagStoreSet measures the cost of populating a fresh TagStore
// with a number of tags drawn from the production distribution.
// The allocs/op figure directly indicates the pivot rate:
//   - 0 allocs → span stayed in inline mode
//   - 1 alloc  → span pivoted to map
//
// Run with different tagStoreInline values to find the break-even capacity.
func BenchmarkTagStoreSet(b *testing.B) {
	workload := productionTagWorkload()
	keys, vals := tagPairs(70)

	b.ResetTimer()
	b.ReportAllocs()
	for i := range b.N {
		n := workload[i%len(workload)]
		var s TagStore[string]
		for j := range n {
			s.Set(keys[j], vals[j])
		}
		// Prevent the compiler from optimising away the store.
		if s.Len() == 0 {
			b.Fatal("unexpected empty store")
		}
	}
}

// BenchmarkTagStoreSetRange measures the write-then-iterate cycle —
// the pattern used during span finish and payload encoding.
func BenchmarkTagStoreSetRange(b *testing.B) {
	workload := productionTagWorkload()
	keys, vals := tagPairs(70)

	b.ResetTimer()
	b.ReportAllocs()
	for i := range b.N {
		n := workload[i%len(workload)]
		var s TagStore[string]
		for j := range n {
			s.Set(keys[j], vals[j])
		}
		s.Range(func(k, v string) bool { return true })
	}
}
