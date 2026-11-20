// JSON value model with Node's JSON.stringify semantics.
//
// Node object literals keep insertion order; Go maps do not. Every response
// body in this forge is built from *Obj (ordered) and []any, and marshalled
// with Marshal, which reproduces JSON.stringify exactly for the values this
// server emits: strings escape " \ and control characters the Node way
// (\b \f \n \r \t, other controls as \u00xx), non-ASCII is raw UTF-8,
// numbers print shortest-round-trip the ECMAScript way.

package main

import (
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Obj is an ordered JSON object: keys in insertion order.
type Obj struct {
	keys []string
	vals []any
}

func NewObj() *Obj { return &Obj{} }

// Set assigns k. An existing key keeps its first position (like a JS
// object literal with a duplicate key: last value wins, first position).
func (o *Obj) Set(k string, v any) *Obj {
	for i, key := range o.keys {
		if key == k {
			o.vals[i] = v
			return o
		}
	}
	o.keys = append(o.keys, k)
	o.vals = append(o.vals, v)
	return o
}

func (o *Obj) Get(k string) (any, bool) {
	for i, key := range o.keys {
		if key == k {
			return o.vals[i], true
		}
	}
	return nil, false
}

func (o *Obj) Len() int { return len(o.keys) }

// O builds an *Obj from alternating key/value pairs. The key/value
// slices are pre-sized to the pair count, so building a response object
// never grows them.
func O(pairs ...any) *Obj {
	o := &Obj{
		keys: make([]string, 0, len(pairs)/2),
		vals: make([]any, 0, len(pairs)/2),
	}
	for i := 0; i+1 < len(pairs); i += 2 {
		if k, ok := pairs[i].(string); ok {
			o.Set(k, pairs[i+1])
		}
	}
	return o
}

// Marshal renders v as JSON, Node-style. Supported: nil, bool, string,
// int, int64, float64, []any, *Obj, map[string]any (keys sorted, like Go).
func Marshal(v any) []byte {
	b := make([]byte, 0, 512)
	return appendJSON(b, v)
}

// MarshalNL renders v as JSON followed by '\n' (every JSON response body
// this server sends ends with a newline). The buffer starts with spare
// capacity so the newline never forces a reallocation for typical
// single-object responses.
func MarshalNL(v any) []byte {
	b := make([]byte, 0, 512)
	b = appendJSON(b, v)
	return append(b, '\n')
}

func appendJSON(dst []byte, v any) []byte {
	switch t := v.(type) {
	case nil:
		return append(dst, "null"...)
	case bool:
		if t {
			return append(dst, "true"...)
		}
		return append(dst, "false"...)
	case string:
		return appendJSONString(dst, t)
	case int:
		return strconv.AppendInt(dst, int64(t), 10)
	case int64:
		return strconv.AppendInt(dst, t, 10)
	case float64:
		return append(dst, formatFloatJS(t)...)
	case []any:
		dst = append(dst, '[')
		for i, e := range t {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = appendJSON(dst, e)
		}
		return append(dst, ']')
	case *Obj:
		if t == nil {
			return append(dst, "null"...)
		}
		dst = append(dst, '{')
		for i, k := range t.keys {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = appendJSONString(dst, k)
			dst = append(dst, ':')
			dst = appendJSON(dst, t.vals[i])
		}
		return append(dst, '}')
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		dst = append(dst, '{')
		for i, k := range keys {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = appendJSONString(dst, k)
			dst = append(dst, ':')
			dst = appendJSON(dst, t[k])
		}
		return append(dst, '}')
	default:
		// Should not happen for values this server builds.
		return append(dst, "null"...)
	}
}

const hexDigits = "0123456789abcdef"

func appendJSONString(dst []byte, s string) []byte {
	dst = append(dst, '"')
	for i := 0; i < len(s); {
		c := s[i]
		switch c {
		case '"':
			dst = append(dst, '\\', '"')
			i++
		case '\\':
			dst = append(dst, '\\', '\\')
			i++
		case '\n':
			dst = append(dst, '\\', 'n')
			i++
		case '\r':
			dst = append(dst, '\\', 'r')
			i++
		case '\t':
			dst = append(dst, '\\', 't')
			i++
		case 0x08:
			dst = append(dst, '\\', 'b')
			i++
		case 0x0c:
			dst = append(dst, '\\', 'f')
			i++
		default:
			if c < 0x20 {
				dst = append(dst, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0x0f])
				i++
			} else if c < 0x80 {
				dst = append(dst, c)
				i++
			} else {
				r, size := utf8.DecodeRuneInString(s[i:])
				if r == utf8.RuneError && size == 1 {
					dst = append(dst, []byte(`\ufffd`)...)
					i++
				} else {
					dst = append(dst, s[i:i+size]...)
					i += size
				}
			}
		}
	}
	return append(dst, '"')
}

// formatFloatJS formats a float64 the way ECMAScript Number::toString does:
// shortest round-trip digits, plain notation for -6 < exponent <= 21
// (decimal point position), exponential otherwise.
func formatFloatJS(f float64) string {
	if f == 0 {
		// -0 prints as "0" in JSON.stringify.
		return "0"
	}
	if f != f {
		return "null" // JSON.stringify(NaN) is null
	}
	if math.IsInf(f, 0) {
		// Infinity: JSON.stringify(Infinity) is null.
		return "null"
	}
	// Get shortest digits + decimal exponent via Go's 'e' format.
	s := strconv.FormatFloat(f, 'e', -1, 64)
	// s looks like "-1.5e+21" or "1e-07".
	neg := false
	if strings.HasPrefix(s, "-") {
		neg = true
		s = s[1:]
	}
	mant, expStr := s, ""
	if idx := strings.IndexByte(s, 'e'); idx >= 0 {
		mant = s[:idx]
		expStr = s[idx+1:]
	}
	exp, _ := strconv.Atoi(expStr)
	digits := strings.Replace(mant, ".", "", 1)
	digits = strings.TrimRight(digits, "0")
	if digits == "" {
		digits = "0"
	}
	// Value = 0.digits * 10^(exp+1); decimal point position n = exp+1.
	n := exp + 1
	var out string
	switch {
	case len(digits) == 1 && digits == "0":
		out = "0"
	case n <= 0:
		// 0.000ddd
		if n >= -6 {
			out = "0." + strings.Repeat("0", -n) + digits
		} else {
			out = digits[:1] + "." + digits[1:] + "e" + jsExp(n-1)
			if len(digits) == 1 {
				out = digits + "e" + jsExp(n-1)
			}
		}
	case n >= len(digits):
		// ddd000
		if n <= 21 {
			out = digits + strings.Repeat("0", n-len(digits))
		} else {
			if len(digits) == 1 {
				out = digits + "e" + jsExp(n-1)
			} else {
				out = digits[:1] + "." + digits[1:] + "e" + jsExp(n-1)
			}
		}
	default:
		// dd.ddd
		out = digits[:n] + "." + digits[n:]
	}
	if neg {
		return "-" + out
	}
	return out
}

func jsExp(e int) string {
	// ECMAScript prints e+21 / e-7 (sign always, no zero padding).
	if e >= 0 {
		return "+" + strconv.Itoa(e)
	}
	return strconv.Itoa(e)
}
