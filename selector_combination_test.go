package jsonpath_test

// Regression materials for "jsonpath: selector combination, placeholder
// binding and failure isolation".
//
// This file adds an independent, restricted reference interpreter for a
// subset of the repository's JSONPath language. The reference model covers
// only: root $, child key (.a), child index ([n], incl. negative), slices
// ([min:max:step]), wildcards (.* / [*]) and recursive descent (..a / ..*).
// Every other syntax (filters, scripts, unions, placeholders, quoted
// brackets, current-node @) is explicitly rejected by the reference model.
//
// All checks enter through the repository's public entry points
// (jsonpath.New, jsonpath.Get, jsonpath.GetWithPaths,
// jsonpath.PlaceholderExtension); no internal final objects are fabricated.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/PaesslerAG/jsonpath"
)

// ---------------------------------------------------------------------------
// restricted reference interpreter (independent model of the supported subset)
// ---------------------------------------------------------------------------

// errRefUnsupported is returned by the reference model for every syntax that
// is outside the covered subset. errRefBudget is returned when the visit
// budget of an ambiguous expansion (wildcard, slice, recursive descent) is
// exhausted. Both are causal, sentinel-wrapped errors.
var (
	errRefUnsupported = errors.New("reference model: unsupported jsonpath syntax")
	errRefBudget      = errors.New("reference model: visit budget exhausted")
)

func refUnsupportedf(format string, args ...interface{}) error {
	return fmt.Errorf("%w: %s", errRefUnsupported, fmt.Sprintf(format, args...))
}

type refSegKind int

const (
	refChildKey refSegKind = iota
	refChildIndex
	refWildcard
	refSlice
	refRecurseKey
	refRecurseAll
)

type refSeg struct {
	kind         refSegKind
	key          string
	idx          int
	min, max, st int
}

func (s refSeg) ambiguous() bool {
	return s.kind == refWildcard || s.kind == refSlice ||
		s.kind == refRecurseKey || s.kind == refRecurseAll
}

// refParse parses the restricted subset and rejects everything else.
func refParse(path string) ([]refSeg, error) {
	if !strings.HasPrefix(path, "$") {
		return nil, refUnsupportedf("path must start with $")
	}
	segs := []refSeg{}
	i := 1
	for i < len(path) {
		switch path[i] {
		case '.':
			if i+1 < len(path) && path[i+1] == '.' {
				i += 2
				if i < len(path) && path[i] == '*' {
					segs = append(segs, refSeg{kind: refRecurseAll})
					i++
					continue
				}
				key, n := refScanIdent(path[i:])
				if n == 0 {
					return nil, refUnsupportedf("recursive descent needs a key or *")
				}
				segs = append(segs, refSeg{kind: refRecurseKey, key: key})
				i += n
				continue
			}
			i++
			if i < len(path) && path[i] == '*' {
				segs = append(segs, refSeg{kind: refWildcard})
				i++
				continue
			}
			key, n := refScanIdent(path[i:])
			if n == 0 {
				return nil, refUnsupportedf("dot selector needs a key or *")
			}
			segs = append(segs, refSeg{kind: refChildKey, key: key})
			i += n
		case '[':
			end := strings.IndexByte(path[i:], ']')
			if end < 0 {
				return nil, refUnsupportedf("unclosed bracket")
			}
			seg, err := refParseBracket(path[i+1 : i+end])
			if err != nil {
				return nil, err
			}
			segs = append(segs, seg)
			i += end + 1
		default:
			return nil, refUnsupportedf("unexpected character %q", path[i])
		}
	}
	return segs, nil
}

func refScanIdent(s string) (string, int) {
	n := 0
	for n < len(s) {
		c := s[n]
		switch {
		case c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z'):
			n++
		case n > 0 && c >= '0' && c <= '9':
			n++
		default:
			return s[:n], n
		}
	}
	return s[:n], n
}

func refParseBracket(inner string) (refSeg, error) {
	if inner == "*" {
		return refSeg{kind: refWildcard}, nil
	}
	if strings.Contains(inner, ":") {
		parts := strings.Split(inner, ":")
		if len(parts) > 3 {
			return refSeg{}, refUnsupportedf("slice has more than 3 parameters")
		}
		seg := refSeg{kind: refSlice, min: 0, max: math.MaxInt32, st: 1}
		defaults := []int{0, math.MaxInt32, 1}
		dst := []*int{&seg.min, &seg.max, &seg.st}
		for i, p := range parts {
			if p == "" {
				*dst[i] = defaults[i]
				continue
			}
			n, err := strconv.Atoi(p)
			if err != nil {
				return refSeg{}, refUnsupportedf("slice parameter %q is not an integer", p)
			}
			*dst[i] = n
		}
		return seg, nil
	}
	n, err := strconv.Atoi(inner)
	if err != nil {
		return refSeg{}, refUnsupportedf("bracket content %q is not in the supported subset", inner)
	}
	return refSeg{kind: refChildIndex, idx: n}, nil
}

type refMatch struct {
	path  string
	value interface{}
}

type refNode struct {
	segs  []string
	value interface{}
}

func refChild(n refNode, key string, v interface{}) refNode {
	segs := make([]string, len(n.segs)+1)
	copy(segs, n.segs)
	segs[len(n.segs)] = key
	return refNode{segs: segs, value: v}
}

// refEval evaluates the parsed segments with a deterministic object traversal
// order (sorted keys) and a hard visit budget for ambiguous expansions.
func refEval(segs []refSeg, root interface{}, budget int) ([]refMatch, error) {
	visits := 0
	hasAmbiguous := false
	for _, s := range segs {
		if s.ambiguous() {
			hasAmbiguous = true
			break
		}
	}

	nodes := []refNode{{value: root}}
	i := 0
	// leading plain run: errors are causal when no ambiguous selector
	// follows; otherwise the repository silently yields an empty match set.
	for i < len(segs) && !segs[i].ambiguous() {
		key, v, err := refApplyPlain(segs[i], nodes[0].value)
		if err != nil {
			if !hasAmbiguous {
				return nil, err
			}
			return []refMatch{}, nil
		}
		nodes[0] = refChild(nodes[0], key, v)
		i++
	}
	for i < len(segs) {
		seg := segs[i]
		i++
		expanded := []refNode{}
		for _, n := range nodes {
			out, err := refExpand(seg, n, &visits, budget)
			if err != nil {
				return nil, err
			}
			expanded = append(expanded, out...)
		}
		run := []refSeg{}
		for i < len(segs) && !segs[i].ambiguous() {
			run = append(run, segs[i])
			i++
		}
		kept := []refNode{}
		for _, n := range expanded {
			cur := n
			ok := true
			for _, ps := range run {
				key, v, err := refApplyPlain(ps, cur.value)
				if err != nil {
					ok = false
					break
				}
				cur = refChild(cur, key, v)
			}
			if ok {
				kept = append(kept, cur)
			}
		}
		nodes = kept
	}
	matches := make([]refMatch, 0, len(nodes))
	for _, n := range nodes {
		matches = append(matches, refMatch{path: refJSONPath(n.segs), value: n.value})
	}
	return matches, nil
}

func refApplyPlain(seg refSeg, v interface{}) (string, interface{}, error) {
	switch seg.kind {
	case refChildKey:
		switch o := v.(type) {
		case map[string]interface{}:
			if r, ok := o[seg.key]; ok {
				return seg.key, r, nil
			}
			return "", nil, fmt.Errorf("unknown key %s", seg.key)
		case []interface{}:
			return "", nil, fmt.Errorf("could not select value, invalid key: %q is not an array index", seg.key)
		default:
			return "", nil, fmt.Errorf("unsupported value type %T for select, expected map[string]interface{}, []interface{} or Array", v)
		}
	case refChildIndex:
		switch o := v.(type) {
		case []interface{}:
			i := seg.idx
			p := i
			if i < 0 {
				p = len(o) + i
			}
			if p < 0 || p >= len(o) {
				return strconv.Itoa(i), nil, nil
			}
			return strconv.Itoa(i), o[p], nil
		case map[string]interface{}:
			k := strconv.Itoa(seg.idx)
			if r, ok := o[k]; ok {
				return k, r, nil
			}
			return "", nil, fmt.Errorf("unknown key %s", k)
		default:
			return "", nil, fmt.Errorf("unsupported value type %T for select, expected map[string]interface{}, []interface{} or Array", v)
		}
	}
	return "", nil, refUnsupportedf("segment kind %d is not plain", seg.kind)
}

func refExpand(seg refSeg, n refNode, visits *int, budget int) ([]refNode, error) {
	charge := func() error {
		*visits++
		if *visits > budget {
			return fmt.Errorf("%w (limit %d)", errRefBudget, budget)
		}
		return nil
	}
	out := []refNode{}
	switch seg.kind {
	case refWildcard:
		switch o := n.value.(type) {
		case []interface{}:
			for i, e := range o {
				if err := charge(); err != nil {
					return nil, err
				}
				out = append(out, refChild(n, strconv.Itoa(i), e))
			}
		case map[string]interface{}:
			for _, k := range refSortedKeys(o) {
				if err := charge(); err != nil {
					return nil, err
				}
				out = append(out, refChild(n, k, o[k]))
			}
		}
	case refSlice:
		o, ok := n.value.([]interface{})
		if !ok {
			return out, nil
		}
		length := len(o)
		min := refNegmax(seg.min, length)
		max := refNegmax(seg.max, length)
		step := seg.st
		if step == 0 {
			step = 1
		}
		if min > max {
			return out, nil
		}
		if step > 0 {
			for i := min; i < max; i += step {
				if err := charge(); err != nil {
					return nil, err
				}
				out = append(out, refChild(n, strconv.Itoa(i), o[i]))
			}
		} else {
			for i := max - 1; i >= min; i += step {
				if err := charge(); err != nil {
					return nil, err
				}
				out = append(out, refChild(n, strconv.Itoa(i), o[i]))
			}
		}
	case refRecurseKey, refRecurseAll:
		enum, err := refEnumerate(n, visits, budget)
		if err != nil {
			return nil, err
		}
		if seg.kind == refRecurseAll {
			for _, node := range enum {
				switch o := node.value.(type) {
				case []interface{}:
					for i, e := range o {
						if err := charge(); err != nil {
							return nil, err
						}
						out = append(out, refChild(node, strconv.Itoa(i), e))
					}
				case map[string]interface{}:
					for _, k := range refSortedKeys(o) {
						if err := charge(); err != nil {
							return nil, err
						}
						out = append(out, refChild(node, k, o[k]))
					}
				}
			}
			return out, nil
		}
		for _, node := range enum {
			o, ok := node.value.(map[string]interface{})
			if !ok {
				continue
			}
			if r, ok := o[seg.key]; ok {
				out = append(out, refChild(node, seg.key, r))
			}
		}
	}
	return out, nil
}

// refEnumerate lists a node and all its descendants in a reproducible
// pre-order (array index order, sorted object keys), bounded by the budget.
func refEnumerate(root refNode, visits *int, budget int) ([]refNode, error) {
	enum := []refNode{}
	var walk func(n refNode) error
	walk = func(n refNode) error {
		*visits++
		if *visits > budget {
			return fmt.Errorf("%w (limit %d)", errRefBudget, budget)
		}
		enum = append(enum, n)
		switch o := n.value.(type) {
		case []interface{}:
			for i, e := range o {
				if err := walk(refChild(n, strconv.Itoa(i), e)); err != nil {
					return err
				}
			}
		case map[string]interface{}:
			for _, k := range refSortedKeys(o) {
				if err := walk(refChild(n, k, o[k])); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(root); err != nil {
		return nil, err
	}
	return enum, nil
}

// refNegmax mirrors the repository's slice bound normalization.
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

func refSortedKeys(o map[string]interface{}) []string {
	keys := make([]string, 0, len(o))
	for k := range o {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// refJSONPath renders canonical access positions exactly like the
// repository's GetWithPaths output: $["seg"]["seg"]...
func refJSONPath(segs []string) string {
	sb := strings.Builder{}
	sb.WriteString("$")
	for _, s := range segs {
		sb.WriteString("[")
		sb.WriteString(strconv.Quote(s))
		sb.WriteString("]")
	}
	return sb.String()
}

func refMatchMap(matches []refMatch) map[string]interface{} {
	m := map[string]interface{}{}
	for _, match := range matches {
		m[match.path] = match.value
	}
	return m
}

// ---------------------------------------------------------------------------
// first-violation reporting helpers
// ---------------------------------------------------------------------------

// pathDiff describes the first divergence between an actual and an expected
// result, keeping the violation position and the necessary context instead of
// a bare boolean.
type pathDiff struct {
	location string
	detail   string
	got      interface{}
	want     interface{}
}

func (d *pathDiff) String() string {
	return fmt.Sprintf("first violation at %s: %s (got %s, want %s)",
		d.location, d.detail, canonicalValue(d.got), canonicalValue(d.want))
}

func canonicalValue(v interface{}) string {
	return fmt.Sprintf("%#v", v)
}

// diffPathMaps returns the first violation between two path->value maps, or
// nil when they are equal. Keys are inspected in sorted order so the reported
// position is stable across runs.
func diffPathMaps(got, want map[string]interface{}) *pathDiff {
	keys := map[string]bool{}
	for k := range got {
		keys[k] = true
	}
	for k := range want {
		keys[k] = true
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	for _, k := range sorted {
		g, gok := got[k]
		w, wok := want[k]
		switch {
		case !gok:
			return &pathDiff{location: k, detail: "position missing in actual result", want: w}
		case !wok:
			return &pathDiff{location: k, detail: "unexpected position in actual result", got: g}
		case !reflect.DeepEqual(g, w):
			return &pathDiff{location: k, detail: "value mismatch", got: g, want: w}
		}
	}
	return nil
}

func mustJSON(t *testing.T, data string) interface{} {
	t.Helper()
	var v interface{}
	if err := json.Unmarshal([]byte(data), &v); err != nil {
		t.Fatalf("could not parse json fixture: %v", err)
	}
	return v
}

func refHasAmbiguous(segs []refSeg) bool {
	for _, s := range segs {
		if s.ambiguous() {
			return true
		}
	}
	return false
}

// canonicalGet renders a Get result in an order-insensitive way for ambiguous
// paths (the repository iterates object maps in random order).
func canonicalGet(segs []refSeg, got interface{}) string {
	if refHasAmbiguous(segs) {
		list, ok := got.([]interface{})
		if !ok {
			return fmt.Sprintf("unexpected non-list ambiguous result %#v", got)
		}
		parts := make([]string, 0, len(list))
		for _, e := range list {
			parts = append(parts, canonicalValue(e))
		}
		sort.Strings(parts)
		return strings.Join(parts, ",")
	}
	return canonicalValue(got)
}

// ---------------------------------------------------------------------------
// combination fixtures: selector level and full path level
// ---------------------------------------------------------------------------

type selectorCombinationCase struct {
	name string
	path string
	data string
	// wantErr is the exact evaluation error message shared by the repository
	// and the reference model; empty means the case must succeed.
	wantErr string
}

func selectorCombinationCases() []selectorCombinationCase {
	return []selectorCombinationCase{
		// array negative index (off-by-one sensitive)
		{name: "negative index selects last element", path: `$[-1]`, data: `[7,8,9]`},
		{name: "negative index selects third from end", path: `$[-3]`, data: `[7,8,9]`},
		{name: "negative index out of bounds yields null", path: `$[-4]`, data: `[7,8,9]`},
		{name: "positive index out of bounds yields null", path: `$[5]`, data: `[1,2]`},
		// slices (end-exclusive endpoint mutations must be caught)
		{name: "slice end is exclusive", path: `$[1:3]`, data: `[0,1,2,3]`},
		{name: "slice from negative start to end", path: `$[-2:]`, data: `[0,1,2,3]`},
		{name: "slice with two negative bounds", path: `$[-3:-1]`, data: `[0,1,2,3]`},
		{name: "slice high bound is clamped", path: `$[2:99]`, data: `[0,1,2,3]`},
		{name: "slice with step", path: `$[0:5:2]`, data: `[0,1,2,3]`},
		{name: "slice default step over all", path: `$[::2]`, data: `[0,1,2,3,4]`},
		{name: "slice reverse", path: `$[::-1]`, data: `[0,1,2]`},
		{name: "slice empty when min above max", path: `$[3:1]`, data: `[0,1,2,3]`},
		{name: "slice on object yields empty set", path: `$[0:2]`, data: `{"0":"a","1":"b"}`},
		// object wildcard
		{name: "object wildcard", path: `$.o.*`, data: `{"o":{"a":1,"b":2,"c":3,"d":4}}`},
		{name: "wildcard on scalar yields empty set", path: `$.a.*`, data: `{"a":5}`},
		{name: "wildcard then key drops missing keys", path: `$.list[*].id`, data: `{"list":[{"id":1},{"x":2},{"id":3}]}`},
		{name: "wildcard then index", path: `$.list[*][0]`, data: `{"list":[[1,2],[3]]}`},
		// recursive descent
		{name: "recursive descent key", path: `$..a`, data: `{"o1":{"a1":{"a":"v"}},"a":"top"}`},
		{name: "recursive descent all", path: `$..*`, data: `{"b":{"x":1},"a":[7,8]}`},
		{name: "recursive descent then index", path: `$..list[0]`, data: `{"list":[1,2],"o":{"list":[3]}}`},
		{name: "recursive descent without match yields empty set", path: `$..zzz`, data: `{"a":{"b":[1,2]}}`},
		// deep empty sets
		{name: "deep empty set through missing prefix", path: `$.missing[*].deep.path`, data: `{"a":1}`},
		{name: "empty array wildcard yields empty set", path: `$.a[*].b`, data: `{"a":[]}`},
		// plain selectors and root
		{name: "root object", path: `$`, data: `{"a":"aa"}`},
		{name: "index into object with numeric key", path: `$[1]`, data: `{"1":"aa"}`},
		{name: "chained plain selectors", path: `$.a.b[2].c`, data: `{"a":{"b":[{"c":1},{"c":2},{"c":3}]}}`},
		{name: "null value is a valid match", path: `$.a`, data: `{"a":null}`},
		// causal evaluation errors (existing error layering)
		{name: "type mismatch keeps causal error", path: `$.a[0]`, data: `{"a":"str"}`,
			wantErr: "unsupported value type string for select, expected map[string]interface{}, []interface{} or Array"},
		{name: "unknown key keeps causal error", path: `$.missing`, data: `{"a":1}`,
			wantErr: "unknown key missing"},
		{name: "unknown numeric key keeps causal error", path: `$[1]`, data: `{"2":"aa"}`,
			wantErr: "unknown key 1"},
	}
}

// TestSelectorCombination_ReferenceModelAgreement feeds parser results
// obtained through the public entry points into the independent restricted
// reference interpreter and compares both values and access positions, at
// selector level (Get) and full path level (GetWithPaths).
func TestSelectorCombination_ReferenceModelAgreement(t *testing.T) {
	for _, tc := range selectorCombinationCases() {
		t.Run(tc.name, func(t *testing.T) {
			data := mustJSON(t, tc.data)

			// parser result through the repository's public entry
			if _, err := jsonpath.New(tc.path); err != nil {
				t.Fatalf("public parser entry rejected supported path %q: %v", tc.path, err)
			}

			segs, err := refParse(tc.path)
			if err != nil {
				t.Fatalf("reference model rejected fixture path %q: %v", tc.path, err)
			}
			refMatches, refErr := refEval(segs, data, 1<<20)

			got, getErr := jsonpath.Get(tc.path, data)
			gotWithPaths, pathsErr := jsonpath.GetWithPaths(tc.path, data)

			if tc.wantErr != "" {
				// rejection must follow the existing error layering and be
				// reproducible: identical input yields the identical error.
				if getErr == nil || getErr.Error() != tc.wantErr {
					t.Fatalf("Get(%q) error = %v, want causal error %q", tc.path, getErr, tc.wantErr)
				}
				if _, again := jsonpath.Get(tc.path, mustJSON(t, tc.data)); again == nil || again.Error() != tc.wantErr {
					t.Fatalf("Get(%q) rejection is not reproducible: %v", tc.path, again)
				}
				if pathsErr == nil || pathsErr.Error() != tc.wantErr {
					t.Fatalf("GetWithPaths(%q) error = %v, want causal error %q", tc.path, pathsErr, tc.wantErr)
				}
				if refErr == nil || refErr.Error() != tc.wantErr {
					t.Fatalf("reference model error = %v, want causal error %q", refErr, tc.wantErr)
				}
				return
			}

			if getErr != nil {
				t.Fatalf("Get(%q) unexpected error: %v", tc.path, getErr)
			}
			if pathsErr != nil {
				t.Fatalf("GetWithPaths(%q) unexpected error: %v", tc.path, pathsErr)
			}
			if refErr != nil {
				t.Fatalf("reference model unexpected error for %q: %v", tc.path, refErr)
			}

			// full path level: values and access positions must match exactly
			wantMap := refMatchMap(refMatches)
			gotMap, ok := gotWithPaths.(map[string]interface{})
			if !ok {
				t.Fatalf("GetWithPaths(%q) returned %T, want map[string]interface{}", tc.path, gotWithPaths)
			}
			if d := diffPathMaps(gotMap, wantMap); d != nil {
				t.Errorf("case %q path %q: %s", tc.name, tc.path, d)
			}

			// selector level: diagnostics (path collection) must not change
			// the original success return
			if refHasAmbiguous(segs) {
				list, ok := got.([]interface{})
				if !ok {
					t.Fatalf("Get(%q) returned %T, want []interface{} for ambiguous path", tc.path, got)
				}
				if len(list) != len(refMatches) {
					t.Fatalf("Get(%q) returned %d matches, reference model has %d", tc.path, len(list), len(refMatches))
				}
				if canonicalGet(segs, got) != canonicalGet(segs, refMapValues(wantMap)) {
					t.Errorf("case %q path %q: selector-level values diverge from path-level values", tc.name, tc.path)
				}
			} else {
				if len(wantMap) != 1 {
					t.Fatalf("reference model produced %d matches for plain path %q", len(wantMap), tc.path)
				}
				for _, want := range wantMap {
					if !reflect.DeepEqual(got, want) {
						t.Errorf("case %q path %q: Get = %s, want %s", tc.name, tc.path, canonicalValue(got), canonicalValue(want))
					}
				}
			}
		})
	}
}

func refMapValues(m map[string]interface{}) []interface{} {
	values := make([]interface{}, 0, len(m))
	for _, v := range m {
		values = append(values, v)
	}
	return values
}
