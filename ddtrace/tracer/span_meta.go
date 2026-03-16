// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2025 Datadog, Inc.

package tracer

import (
	"fmt"
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

// String returns a merged map representation (m + promoted attrs) for debug logging.
func (sm spanMeta) String() string {
	var b strings.Builder
	b.WriteString("map[")
	first := true
	for k, v := range sm.m {
		if !first {
			b.WriteByte(' ')
		}
		first = false
		fmt.Fprintf(&b, "%s:%s", k, v)
	}
	sm.attrs.ForEach(func(name, val string) {
		if !first {
			b.WriteByte(' ')
		}
		first = false
		fmt.Fprintf(&b, "%s:%s", name, val)
	})
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
	var encErr error
	sm.attrs.ForEach(func(name, val string) {
		if encErr != nil {
			return
		}
		if err := en.WriteString(name); err != nil {
			encErr = msgp.WrapError(err, "Meta")
			return
		}
		if err := en.WriteString(val); err != nil {
			encErr = msgp.WrapError(err, "Meta", name)
		}
	})
	return encErr
}

// DecodeMsg reads a msgp map and routes promoted keys into attrs,
// everything else into m.
func (sm *spanMeta) DecodeMsg(dc *msgp.Reader) error {
	header, err := dc.ReadMapHeader()
	if err != nil {
		return msgp.WrapError(err, "Meta")
	}
	// Reuse sm.m and sm.attrs if already allocated (pool/reuse pattern from generated
	// msgp code); otherwise allocate fresh. This avoids one heap allocation per decoded
	// span in environments where Span objects are recycled between decode calls.
	if sm.m != nil {
		clear(sm.m)
	} else {
		sm.m = make(map[string]string, header)
	}
	sm.attrs.Reset() // nil-safe: clears bits if set, no-op if nil
	for range header {
		key, err := dc.ReadString()
		if err != nil {
			return msgp.WrapError(err, "Meta")
		}
		val, err := dc.ReadString()
		if err != nil {
			return msgp.WrapError(err, "Meta", key)
		}
		if ak, ok := tinternal.AttrKeyForTag(key); ok {
			if sm.attrs == nil {
				sm.attrs = new(tinternal.SpanAttributes)
			}
			sm.attrs.Set(ak, val)
		} else {
			sm.m[key] = val
		}
	}
	return nil
}

// Msgsize returns an upper bound estimate of the serialized size.
func (sm *spanMeta) Msgsize() int {
	size := msgp.MapHeaderSize
	for k, v := range sm.m {
		size += msgp.StringPrefixSize + len(k) + msgp.StringPrefixSize + len(v)
	}
	sm.attrs.ForEach(func(name, val string) {
		size += msgp.StringPrefixSize + len(name) + msgp.StringPrefixSize + len(val)
	})
	return size
}
