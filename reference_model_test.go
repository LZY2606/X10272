package jsonpath_test

// This file contains an independent, restricted reference interpreter for the
// JSONPath subset supported by this repository. It shares no code with the
// production implementation: tests parse a path through the public entry
// points (jsonpath.New / jsonpath.Get / jsonpath.GetWithPaths) and compare
// values and access positions against this reference model.
//
// The reference model deliberately
//   - covers only the supported subset: root $, dot children, array indices
//     (including negative indices), slices [min:max:step], wildcards, unions
//     of indices and recursive descent (..key / ..*),
//   - explicitly rejects every other syntax (filters, scripts, placeholders,
//     quoted keys, ...) instead of guessing semantics,
//   - traverses object keys in sorted order so recursive descent and wildcard
//     enumeration are reproducible,
//   - enforces a visit budget on recursive descent so runaway inputs surface
//     a causal "budget exhausted" error,
//   - honours context cancellation with the causal context error.

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

type refKind int

const (
	refChildKey refKind = iota
	refChildIndex
	refSlice
	refWildcard
	refUnion
	refRecurseKey
	refRecurseWildcard
)

type refSegment struct {
	kind  refKind
	key   string
	index int
	min   int
	max   int
	step  int
	union []refSegment
}

func (s refSegment) ambiguous() bool {
	switch s.kind {
	case refSlice, refWildcard, refUnion, refRecurseKey, refRecurseWildcard:
		return true
	default:
		return false
	}
}

func refUnsupported(format string, args ...interface{}) error {
	return fmt.Errorf("reference model: unsupported syntax: "+format, args...)
}

func refScanIdent(s string) (string, string) {
	i := 0
	for i < len(s) {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' {
			i++
			continue
		}
		break
	}
	return s[:i], s[i:]
}

func refParseBracket(content string) (refSegment, error) {
	content = strings.TrimSpace(content)
	if content == "*" {
		return refSegment{kind: refWildcard}, nil
	}
	if strings.Contains(content, ":") {
		parts := strings.Split(content, ":")
		if len(parts) > 3 {
			return refSegment{}, refUnsupported("slice %q has more than 3 parameters", content)
		}
		vals := []int{0, math.MaxInt32, 1}
		for i, p := range parts {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			n, err := strconv.Atoi(p)
			if err != nil {
				return refSegment{}, refUnsupported("slice parameter %q is not an integer", p)
			}
			vals[i] = n
		}
		return refSegment{kind: refSlice, min: vals[0], max: vals[1], step: vals[2]}, nil
	}
	if strings.Contains(content, ",") {
		parts := strings.Split(content, ",")
		union := []refSegment{}
		for _, p := range parts {
			n, err := strconv.Atoi(strings.TrimSpace(p))
			if err != nil {
				return refSegment{}, refUnsupported("union member %q is not an integer", p)
			}
			union = append(union, refSegment{kind: refChildIndex, index: n})
		}
		return refSegment{kind: refUnion, union: union}, nil
	}
	n, err := strconv.Atoi(content)
	if err != nil {
		return refSegment{}, refUnsupported("bracket content %q is not an integer index", content)
	}
	return refSegment{kind: refChildIndex, index: n}, nil
}

// parseReference parses the restricted JSONPath subset. Any syntax outside
// the subset supported by this repository is rejected explicitly.
func parseReference(path string) ([]refSegment, error) {
	if path == "" || path[0] != '$' {
		return nil, refUnsupported("path %q does not start with root $", path)
	}
	rest := path[1:]
	segs := []refSegment{}
	for len(rest) > 0 {
		switch rest[0] {
		case '.':
			rest = rest[1:]
			switch {
			case strings.HasPrefix(rest, "."):
				rest = rest[1:]
				if strings.HasPrefix(rest, "*") {
					segs = append(segs, refSegment{kind: refRecurseWildcard})
					rest = rest[1:]
					continue
				}
				ident, r := refScanIdent(rest)
				if ident == "" {
					return nil, refUnsupported("recursive descent in %q", path)
				}
				segs = append(segs, refSegment{kind: refRecurseKey, key: ident})
				rest = r
			case strings.HasPrefix(rest, "*"):
				segs = append(segs, refSegment{kind: refWildcard})
				rest = rest[1:]
			default:
				ident, r := refScanIdent(rest)
				if ident == "" {
					return nil, refUnsupported("dot selector in %q", path)
				}
				segs = append(segs, refSegment{kind: refChildKey, key: ident})
				rest = r
			}
		case '[':
			end := strings.IndexByte(rest, ']')
			if end < 0 {
				return nil, refUnsupported("unclosed bracket in %q", path)
			}
			seg, err := refParseBracket(rest[1:end])
			if err != nil {
				return nil, err
			}
			segs = append(segs, seg)
			rest = rest[end+1:]
		default:
			return nil, refUnsupported("unexpected character %q in %q", string(rest[0]), path)
		}
	}
	return segs, nil
}

type refMatch struct {
	keys  []string
	value interface{}
}

const refDefaultBudget = 100000

type refEvaluator struct {
	budget int
	visits int
}

func (e *refEvaluator) spend() error {
	e.visits++
	limit := e.budget
	if limit <= 0 {
		limit = refDefaultBudget
	}
	if e.visits > limit {
		return fmt.Errorf("reference model: visit budget exhausted after %d visits", limit)
	}
	return nil
}

func refAppendKey(keys []string, key string) []string {
	out := make([]string, len(keys), len(keys)+1)
	copy(out, keys)
	return append(out, key)
}

// refChildKeys returns child keys in a reproducible order: arrays by index,
// objects sorted by key. Scalars have no children.
func refChildKeys(v interface{}) []string {
	switch node := v.(type) {
	case []interface{}:
		keys := make([]string, len(node))
		for i := range node {
			keys[i] = strconv.Itoa(i)
		}
		return keys
	case map[string]interface{}:
		keys := make([]string, 0, len(node))
		for k := range node {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return keys
	default:
		return nil
	}
}

func refChild(v interface{}, key string) interface{} {
	switch node := v.(type) {
	case []interface{}:
		i, err := strconv.Atoi(key)
		if err != nil || i < 0 || i >= len(node) {
			return nil
		}
		return node[i]
	case map[string]interface{}:
		return node[key]
	default:
		return nil
	}
}

// refSelect mirrors the repository's direct selector semantics, including
// negative index wrapping and the out-of-range nil result for arrays.
func refSelect(seg refSegment, v interface{}) (string, interface{}, error) {
	switch node := v.(type) {
	case []interface{}:
		if seg.kind != refChildIndex {
			return "", nil, fmt.Errorf("could not select value, invalid key: %q is not an array index", seg.key)
		}
		p := seg.index
		if p < 0 {
			p = len(node) + p
		}
		if p < 0 || p >= len(node) {
			return strconv.Itoa(seg.index), nil, nil
		}
		return strconv.Itoa(seg.index), node[p], nil
	case map[string]interface{}:
		key := seg.key
		if seg.kind == refChildIndex {
			key = strconv.Itoa(seg.index)
		}
		value, ok := node[key]
		if !ok {
			return "", nil, fmt.Errorf("unknown key %s", key)
		}
		return key, value, nil
	default:
		return "", nil, fmt.Errorf("unsupported value type %T for select, expected map[string]interface{}, []interface{} or Array", v)
	}
}

func refNegmax(n, max int) int {
	if n < 0 {
		n = max + n
		if n < 0 {
			n = 0
		}
	} else if n > max {
		return max
	}
	return n
}

// refSliceIndices mirrors the repository's range selector: max is exclusive,
// negative endpoints wrap, and a negative step iterates downwards.
func refSliceIndices(min, max, step, n int) []int {
	if step == 0 {
		step = 1
	}
	min = refNegmax(min, n)
	max = refNegmax(max, n)
	if min > max {
		return nil
	}
	indices := []int{}
	if step > 0 {
		for i := min; i < max; i += step {
			indices = append(indices, i)
		}
	} else {
		for i := max - 1; i >= min; i += step {
			indices = append(indices, i)
		}
	}
	return indices
}

// walk visits v and all descendants in preorder with sorted object keys,
// spending one budget unit and checking cancellation per node.
func (e *refEvaluator) walk(ctx context.Context, keys []string, v interface{}, fn func([]string, interface{})) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := e.spend(); err != nil {
		return err
	}
	fn(keys, v)
	for _, ck := range refChildKeys(v) {
		if err := e.walk(ctx, refAppendKey(keys, ck), refChild(v, ck), fn); err != nil {
			return err
		}
	}
	return nil
}

func (e *refEvaluator) expand(ctx context.Context, seg refSegment, m refMatch, ambiguous bool) ([]refMatch, error) {
	single := func(key string, value interface{}) []refMatch {
		return []refMatch{{keys: refAppendKey(m.keys, key), value: value}}
	}
	switch seg.kind {
	case refChildKey, refChildIndex:
		key, value, err := refSelect(seg, m.value)
		if err != nil {
			if ambiguous {
				return nil, nil
			}
			return nil, err
		}
		return single(key, value), nil
	case refUnion:
		out := []refMatch{}
		for _, u := range seg.union {
			key, value, err := refSelect(u, m.value)
			if err != nil {
				continue
			}
			out = append(out, refMatch{refAppendKey(m.keys, key), value})
		}
		return out, nil
	case refWildcard:
		if err := e.spend(); err != nil {
			return nil, err
		}
		out := []refMatch{}
		for _, ck := range refChildKeys(m.value) {
			out = append(out, refMatch{refAppendKey(m.keys, ck), refChild(m.value, ck)})
		}
		return out, nil
	case refSlice:
		arr, ok := m.value.([]interface{})
		if !ok {
			return nil, nil
		}
		out := []refMatch{}
		for _, i := range refSliceIndices(seg.min, seg.max, seg.step, len(arr)) {
			out = append(out, refMatch{refAppendKey(m.keys, strconv.Itoa(i)), arr[i]})
		}
		return out, nil
	case refRecurseKey:
		out := []refMatch{}
		err := e.walk(ctx, m.keys, m.value, func(keys []string, v interface{}) {
			key, value, err := refSelect(refSegment{kind: refChildKey, key: seg.key}, v)
			if err != nil {
				return
			}
			out = append(out, refMatch{refAppendKey(keys, key), value})
		})
		if err != nil {
			return nil, err
		}
		return out, nil
	case refRecurseWildcard:
		out := []refMatch{}
		err := e.walk(ctx, m.keys, m.value, func(keys []string, v interface{}) {
			for _, ck := range refChildKeys(v) {
				out = append(out, refMatch{refAppendKey(keys, ck), refChild(v, ck)})
			}
		})
		if err != nil {
			return nil, err
		}
		return out, nil
	default:
		return nil, fmt.Errorf("reference model: unknown segment kind %d", seg.kind)
	}
}

// evaluateReference evaluates the parsed segments and returns all matches
// with their access positions, plus whether the path is ambiguous (i.e.
// yields a list of matches rather than a single value).
func evaluateReference(ctx context.Context, segs []refSegment, root interface{}, budget int) ([]refMatch, bool, error) {
	e := &refEvaluator{budget: budget}
	matches := []refMatch{{value: root}}
	ambiguous := false
	for _, seg := range segs {
		if seg.ambiguous() {
			ambiguous = true
		}
		next := []refMatch{}
		for _, m := range matches {
			expanded, err := e.expand(ctx, seg, m, ambiguous)
			if err != nil {
				return nil, ambiguous, err
			}
			next = append(next, expanded...)
		}
		matches = next
	}
	return matches, ambiguous, nil
}

// refPathString renders access positions in the same $["key"]... shape the
// repository produces for GetWithPaths.
func refPathString(keys []string) string {
	sb := strings.Builder{}
	sb.WriteString("$")
	for _, k := range keys {
		sb.WriteString("[")
		sb.WriteString(strconv.Quote(k))
		sb.WriteString("]")
	}
	return sb.String()
}
