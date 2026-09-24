package jsonpath_test

// This file contains an independent, restricted reference interpreter for
// the JSONPath subset supported by this repository. It deliberately shares
// no code with the implementation under test: it parses and evaluates on
// its own so that selector-level and full-path-level composition tests can
// compare values and access positions (paths) against a second opinion.
//
// Supported subset (anything else is explicitly rejected):
//   $                 root
//   .key              dot child (Go identifier syntax)
//   [i]               array index, negative allowed; on objects key "i"
//   [min:max[:step]]  array slice, negative endpoints allowed
//   .* / [*]          wildcard over object (sorted keys) or array
//   ..key / ..*       recursive descent (pre-order, sorted object keys)
//
// Explicitly rejected: quoted bracket keys, unions, filters, scripts,
// placeholders, the current element selector @ and any trailing garbage.

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

type refKind int

const (
	refChild refKind = iota
	refIndex
	refSlice
	refWildcard
	refRecursiveChild
	refRecursiveWildcard
)

type refSegment struct {
	kind    refKind
	key     string
	index   int
	min     int
	max     int
	step    int
	hasMin  bool
	hasMax  bool
	hasStep bool
}

func (s refSegment) ambiguous() bool {
	switch s.kind {
	case refSlice, refWildcard, refRecursiveChild, refRecursiveWildcard:
		return true
	}
	return false
}

type refPath []refSegment

func parseReference(path string) (refPath, error) {
	if !strings.HasPrefix(path, "$") {
		return nil, fmt.Errorf("reference model: path %q does not start with $", path)
	}
	rest := path[1:]
	segs := refPath{}
	for len(rest) > 0 {
		switch {
		case strings.HasPrefix(rest, ".."):
			rest = rest[2:]
			if strings.HasPrefix(rest, "*") {
				segs = append(segs, refSegment{kind: refRecursiveWildcard})
				rest = rest[1:]
				continue
			}
			ident, r := scanRefIdent(rest)
			if ident == "" {
				return nil, fmt.Errorf("reference model: unsupported recursive descent selector in %q", path)
			}
			segs = append(segs, refSegment{kind: refRecursiveChild, key: ident})
			rest = r
		case strings.HasPrefix(rest, "."):
			rest = rest[1:]
			if strings.HasPrefix(rest, "*") {
				segs = append(segs, refSegment{kind: refWildcard})
				rest = rest[1:]
				continue
			}
			ident, r := scanRefIdent(rest)
			if ident == "" {
				return nil, fmt.Errorf("reference model: unsupported dot selector in %q", path)
			}
			segs = append(segs, refSegment{kind: refChild, key: ident})
			rest = r
		case strings.HasPrefix(rest, "["):
			end := strings.Index(rest, "]")
			if end < 0 {
				return nil, fmt.Errorf("reference model: unclosed bracket in %q", path)
			}
			seg, err := parseRefBracket(rest[1:end], path)
			if err != nil {
				return nil, err
			}
			segs = append(segs, seg)
			rest = rest[end+1:]
		default:
			return nil, fmt.Errorf("reference model: unsupported syntax %q in %q", rest, path)
		}
	}
	return segs, nil
}

func scanRefIdent(s string) (string, string) {
	i := 0
	for i < len(s) {
		c := s[i]
		isAlpha := c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
		isDigit := c >= '0' && c <= '9'
		if !isAlpha && !(isDigit && i > 0) {
			break
		}
		i++
	}
	return s[:i], s[i:]
}

func parseRefBracket(inner, path string) (refSegment, error) {
	reject := func() (refSegment, error) {
		return refSegment{}, fmt.Errorf("reference model: unsupported bracket selector [%s] in %q", inner, path)
	}
	if inner == "*" {
		return refSegment{kind: refWildcard}, nil
	}
	if strings.ContainsAny(inner, ",?'\"()#@") {
		return reject()
	}
	if strings.Contains(inner, ":") {
		parts := strings.Split(inner, ":")
		if len(parts) < 2 || len(parts) > 3 {
			return reject()
		}
		seg := refSegment{kind: refSlice}
		nums := []*int{&seg.min, &seg.max, &seg.step}
		flags := []*bool{&seg.hasMin, &seg.hasMax, &seg.hasStep}
		for i, p := range parts {
			if p == "" {
				continue
			}
			n, err := strconv.Atoi(p)
			if err != nil {
				return reject()
			}
			*nums[i] = n
			*flags[i] = true
		}
		return seg, nil
	}
	n, err := strconv.Atoi(inner)
	if err != nil {
		return reject()
	}
	return refSegment{kind: refIndex, index: n}, nil
}

// refNode is a value together with its normalized access position.
type refNode struct {
	path  string
	value interface{}
}

func refQuoteKey(key string) string {
	return fmt.Sprintf("[%s]", strconv.Quote(fmt.Sprint(key)))
}

// evalReference evaluates segs on data and returns all matches in
// deterministic traversal order. ambiguous reports whether the path
// contains any wildcard selector, in which case the repository evaluates
// to a list of matches. Selector failures are only fatal while the path
// is still unambiguous; afterwards they drop the affected branch, mirroring
// the repository's matcher semantics.
func evalReference(segs refPath, data interface{}) (matches []refNode, ambiguous bool, err error) {
	nodes := []refNode{{path: "$", value: data}}
	for _, seg := range segs {
		if seg.ambiguous() {
			ambiguous = true
		}
		plain := !ambiguous
		next := []refNode{}
		for _, n := range nodes {
			var out []refNode
			var err error
			switch seg.kind {
			case refChild:
				out, err = refSelectChild(n, seg.key, plain)
			case refIndex:
				out, err = refSelectIndex(n, seg.index, plain)
			case refSlice:
				out = refSelectSlice(n, seg)
			case refWildcard:
				out = refSelectWildcard(n)
			case refRecursiveChild:
				for _, d := range refDescendants(n) {
					m, _ := refSelectChild(d, seg.key, false)
					out = append(out, m...)
				}
			case refRecursiveWildcard:
				for _, d := range refDescendants(n) {
					out = append(out, refSelectWildcard(d)...)
				}
			}
			if err != nil {
				return nil, ambiguous, err
			}
			next = append(next, out...)
		}
		nodes = next
	}
	return nodes, ambiguous, nil
}

func refSelectChild(n refNode, key string, plain bool) ([]refNode, error) {
	o, ok := n.value.(map[string]interface{})
	if !ok {
		if plain {
			return nil, fmt.Errorf("unsupported value type %T for select, expected map[string]interface{}, []interface{} or Array", n.value)
		}
		return nil, nil
	}
	v, ok := o[key]
	if !ok {
		if plain {
			return nil, fmt.Errorf("unknown key %s", key)
		}
		return nil, nil
	}
	return []refNode{{path: n.path + refQuoteKey(key), value: v}}, nil
}

func refSelectIndex(n refNode, i int, plain bool) ([]refNode, error) {
	switch o := n.value.(type) {
	case []interface{}:
		p := i
		if i < 0 {
			p = len(o) + i
		}
		key := strconv.Itoa(i)
		if p < 0 || p >= len(o) {
			return []refNode{{path: n.path + refQuoteKey(key), value: nil}}, nil
		}
		return []refNode{{path: n.path + refQuoteKey(key), value: o[p]}}, nil
	case map[string]interface{}:
		return refSelectChild(n, strconv.Itoa(i), plain)
	default:
		if plain {
			return nil, fmt.Errorf("unsupported value type %T for select, expected map[string]interface{}, []interface{} or Array", n.value)
		}
		return nil, nil
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

func refSelectSlice(n refNode, seg refSegment) []refNode {
	o, ok := n.value.([]interface{})
	if !ok {
		return nil
	}
	min, max, step := 0, len(o), 1
	if seg.hasMin {
		min = seg.min
	}
	if seg.hasMax {
		max = seg.max
	}
	if seg.hasStep {
		step = seg.step
	}
	if step == 0 {
		step = 1
	}
	min = refNegmax(min, len(o))
	max = refNegmax(max, len(o))
	if min > max {
		return nil
	}
	out := []refNode{}
	if step > 0 {
		for i := min; i < max; i += step {
			out = append(out, refNode{path: n.path + refQuoteKey(strconv.Itoa(i)), value: o[i]})
		}
	} else {
		for i := max - 1; i >= min; i += step {
			out = append(out, refNode{path: n.path + refQuoteKey(strconv.Itoa(i)), value: o[i]})
		}
	}
	return out
}

func refSelectWildcard(n refNode) []refNode {
	switch o := n.value.(type) {
	case []interface{}:
		out := make([]refNode, 0, len(o))
		for i, v := range o {
			out = append(out, refNode{path: n.path + refQuoteKey(strconv.Itoa(i)), value: v})
		}
		return out
	case map[string]interface{}:
		keys := make([]string, 0, len(o))
		for k := range o {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := make([]refNode, 0, len(o))
		for _, k := range keys {
			out = append(out, refNode{path: n.path + refQuoteKey(k), value: o[k]})
		}
		return out
	}
	return nil
}

// refDescendants returns n and all its descendants in pre-order, visiting
// object children in sorted key order so the traversal is reproducible.
func refDescendants(n refNode) []refNode {
	out := []refNode{n}
	for _, c := range refSelectWildcard(n) {
		out = append(out, refDescendants(c)...)
	}
	return out
}
