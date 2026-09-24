package jsonpath_test

// This file contains an independent, restricted reference interpreter for
// the JSONPath subset this repository supports. It is used to cross-check
// the production parser/evaluator through the public entry points
// (jsonpath.New, jsonpath.Get, jsonpath.GetWithPaths). The reference model
// deliberately covers ONLY the supported subset and rejects every other
// syntax with refUnsupportedError. It shares no code with the production
// implementation; traversal is deterministic (sorted object keys) and
// recursive descent enforces an explicit visit budget, mirroring the
// production guarantees under test.

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// refUnsupportedError reports syntax outside the supported subset.
type refUnsupportedError struct{ syntax string }

func (e refUnsupportedError) Error() string {
	return fmt.Sprintf("reference model: unsupported syntax %q", e.syntax)
}

// errRefVisitBudget mirrors jsonpath.ErrVisitBudgetExhausted.
var errRefVisitBudget = errors.New("reference model: recursive descent visit budget exhausted")

type refSegKind int

const (
	refKey refSegKind = iota
	refIndex
	refUnion
	refSlice
	refStar
	refRecurse
)

type refUnionKey struct {
	isIndex bool
	index   int
	key     string
}

type refSegment struct {
	kind  refSegKind
	key   string
	index int
	union []refUnionKey
	min   int
	max   int
	step  int
}

func (s refSegment) ambiguous() bool {
	switch s.kind {
	case refUnion, refSlice, refStar, refRecurse:
		return true
	}
	return false
}

func refIdent(r string) (string, int) {
	i := 0
	for i < len(r) {
		c := r[i]
		if c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (i > 0 && c >= '0' && c <= '9') {
			i++
			continue
		}
		break
	}
	return r[:i], i
}

// refParse parses the restricted subset: $, .key, [i], ["k"], [k1,k2,..],
// [min:max:step], .*, [*], ..key, ..*, ..[k1,k2,..]. Everything else is
// rejected with refUnsupportedError.
func refParse(path string) ([]refSegment, error) {
	if path == "" || path[0] != '$' {
		return nil, refUnsupportedError{path}
	}
	segs := []refSegment{}
	i := 1
	for i < len(path) {
		switch path[i] {
		case '.':
			if i+1 < len(path) && path[i+1] == '.' {
				segs = append(segs, refSegment{kind: refRecurse})
				i += 2
				if i < len(path) && path[i] == '[' {
					inner, n, err := refParseBracket(path[i:])
					if err != nil {
						return nil, err
					}
					if inner.kind == refSlice {
						// the repository rejects mapper combined with range query
						return nil, refUnsupportedError{path}
					}
					segs = append(segs, inner)
					i += n
					continue
				}
				if i < len(path) && path[i] == '*' {
					segs = append(segs, refSegment{kind: refStar})
					i++
					continue
				}
				ident, n := refIdent(path[i:])
				if n == 0 {
					return nil, refUnsupportedError{path}
				}
				segs = append(segs, refSegment{kind: refKey, key: ident})
				i += n
				continue
			}
			if i+1 < len(path) && path[i+1] == '*' {
				segs = append(segs, refSegment{kind: refStar})
				i += 2
				continue
			}
			ident, n := refIdent(path[i+1:])
			if n == 0 {
				return nil, refUnsupportedError{path}
			}
			segs = append(segs, refSegment{kind: refKey, key: ident})
			i += 1 + n
		case '[':
			seg, n, err := refParseBracket(path[i:])
			if err != nil {
				return nil, err
			}
			segs = append(segs, seg)
			i += n
		default:
			return nil, refUnsupportedError{path}
		}
	}
	return segs, nil
}

func refParseBracket(r string) (refSegment, int, error) {
	end := strings.IndexByte(r, ']')
	if end < 0 {
		return refSegment{}, 0, refUnsupportedError{r}
	}
	body := r[1:end]
	if body == "*" {
		return refSegment{kind: refStar}, end + 1, nil
	}
	if strings.Contains(body, ":") {
		parts := strings.Split(body, ":")
		if len(parts) > 3 {
			return refSegment{}, 0, refUnsupportedError{r}
		}
		def := []int{0, math.MaxInt32, 1}
		vals := make([]int, 3)
		for i, p := range parts {
			if p == "" {
				vals[i] = def[i]
				continue
			}
			n, err := strconv.Atoi(p)
			if err != nil {
				return refSegment{}, 0, refUnsupportedError{r}
			}
			vals[i] = n
		}
		for i := len(parts); i < 3; i++ {
			vals[i] = def[i]
		}
		return refSegment{kind: refSlice, min: vals[0], max: vals[1], step: vals[2]}, end + 1, nil
	}
	items := strings.Split(body, ",")
	keys := make([]refUnionKey, 0, len(items))
	for _, item := range items {
		item = strings.TrimSpace(item)
		if n, err := strconv.Atoi(item); err == nil {
			keys = append(keys, refUnionKey{isIndex: true, index: n})
			continue
		}
		if len(item) >= 2 && item[0] == '"' && item[len(item)-1] == '"' && !strings.Contains(item[1:len(item)-1], `"`) {
			keys = append(keys, refUnionKey{key: item[1 : len(item)-1]})
			continue
		}
		return refSegment{}, 0, refUnsupportedError{r}
	}
	if len(keys) == 1 {
		if keys[0].isIndex {
			return refSegment{kind: refIndex, index: keys[0].index}, end + 1, nil
		}
		return refSegment{kind: refKey, key: keys[0].key}, end + 1, nil
	}
	return refSegment{kind: refUnion, union: keys}, end + 1, nil
}

type refMatch struct {
	segs  []string
	value interface{}
}

func refAppendSeg(segs []string, key string) []string {
	next := make([]string, len(segs)+1)
	copy(next, segs)
	next[len(segs)] = key
	return next
}

// refSelectValue mirrors selectValue for []interface{} and
// map[string]interface{} values, including raw (unresolved) index keys and
// nil matches for out-of-range indices.
func refSelectValue(v interface{}, key refUnionKey) (value interface{}, jkey string, err error) {
	switch o := v.(type) {
	case []interface{}:
		if !key.isIndex {
			return nil, "", fmt.Errorf("could not select value, invalid key: expected number but got %s (string)", key.key)
		}
		i := key.index
		p := i
		if i < 0 {
			p = len(o) + i
		}
		if p < 0 || p >= len(o) {
			return nil, strconv.Itoa(i), nil
		}
		return o[p], strconv.Itoa(i), nil
	case map[string]interface{}:
		k := key.key
		if key.isIndex {
			k = strconv.Itoa(key.index)
		}
		if r, ok := o[k]; ok {
			return r, k, nil
		}
		return nil, "", fmt.Errorf("unknown key %s", k)
	default:
		return nil, "", fmt.Errorf("unsupported value type %T for select, expected map[string]interface{}, []interface{} or Array", v)
	}
}

func refChildren(v interface{}) []refMatch {
	switch o := v.(type) {
	case []interface{}:
		out := make([]refMatch, 0, len(o))
		for i, e := range o {
			out = append(out, refMatch{segs: []string{strconv.Itoa(i)}, value: e})
		}
		return out
	case map[string]interface{}:
		keys := make([]string, 0, len(o))
		for k := range o {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := make([]refMatch, 0, len(o))
		for _, k := range keys {
			out = append(out, refMatch{segs: []string{k}, value: o[k]})
		}
		return out
	}
	return nil
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

func refRecurseNodes(m refMatch, budget int) ([]refMatch, error) {
	var out []refMatch
	var walk func(segs []string, v interface{}) error
	walk = func(segs []string, v interface{}) error {
		if budget < 1 {
			return errRefVisitBudget
		}
		budget--
		out = append(out, refMatch{segs: segs, value: v})
		for _, c := range refChildren(v) {
			if err := walk(refAppendSeg(segs, c.segs[0]), c.value); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(m.segs, m.value); err != nil {
		return nil, err
	}
	return out, nil
}

// refEval evaluates parsed segments. Plain segment failures abort the
// evaluation while no ambiguous segment has been seen (plain prefix);
// afterwards they only drop the affected match, mirroring the production
// path structure.
func refEval(segs []refSegment, data interface{}, budget int) ([]refMatch, error) {
	matches := []refMatch{{value: data}}
	ambiguous := false
	for _, seg := range segs {
		next := []refMatch{}
		switch seg.kind {
		case refKey, refIndex:
			key := refUnionKey{isIndex: seg.kind == refIndex, index: seg.index, key: seg.key}
			for _, m := range matches {
				v, jkey, err := refSelectValue(m.value, key)
				if err != nil {
					if !ambiguous {
						return nil, err
					}
					continue
				}
				next = append(next, refMatch{segs: refAppendSeg(m.segs, jkey), value: v})
			}
		case refStar:
			ambiguous = true
			for _, m := range matches {
				for _, c := range refChildren(m.value) {
					next = append(next, refMatch{segs: refAppendSeg(m.segs, c.segs[0]), value: c.value})
				}
			}
		case refUnion:
			ambiguous = true
			for _, m := range matches {
				for _, k := range seg.union {
					v, jkey, err := refSelectValue(m.value, k)
					if err != nil {
						continue
					}
					next = append(next, refMatch{segs: refAppendSeg(m.segs, jkey), value: v})
				}
			}
		case refSlice:
			ambiguous = true
			for _, m := range matches {
				o, ok := m.value.([]interface{})
				if !ok {
					continue
				}
				n := len(o)
				min := refNegmax(seg.min, n)
				max := refNegmax(seg.max, n)
				step := seg.step
				if step == 0 {
					step = 1
				}
				if min > max {
					continue
				}
				if step > 0 {
					for i := min; i < max; i += step {
						next = append(next, refMatch{segs: refAppendSeg(m.segs, strconv.Itoa(i)), value: o[i]})
					}
				} else {
					for i := max - 1; i >= min; i += step {
						next = append(next, refMatch{segs: refAppendSeg(m.segs, strconv.Itoa(i)), value: o[i]})
					}
				}
			}
		case refRecurse:
			ambiguous = true
			for _, m := range matches {
				nodes, err := refRecurseNodes(m, budget)
				if err != nil {
					return nil, err
				}
				next = append(next, nodes...)
			}
		}
		matches = next
	}
	return matches, nil
}

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

func refHasAmbiguous(segs []refSegment) bool {
	for _, s := range segs {
		if s.ambiguous() {
			return true
		}
	}
	return false
}

// TestReferenceModelRejectsUnsupportedSyntax proves the reference model
// covers only the supported subset and explicitly rejects everything else.
func TestReferenceModelRejectsUnsupportedSyntax(t *testing.T) {
	unsupported := []string{
		"",                  // empty
		".a",                // missing root
		"@.a",               // current element root
		"$['a']",            // single quoted key
		"$[?(@.a==1)]",      // filter
		"$[(@.length-1)]",   // script expression in brackets
		"$.a(@==1)",         // script
		"{#0: $[*]}",        // placeholder object
		"$..[1:2]",          // mapper combined with range query
		"$[1:2:3:4]",        // too many slice parameters
		"$[1,2:3]",          // mixed union and slice
		"$.a b",             // trailing garbage
		"$..",               // dangling recursive descent
	}
	for _, path := range unsupported {
		if segs, err := refParse(path); err == nil {
			t.Errorf("reference model accepted unsupported syntax %q as %+v", path, segs)
		} else {
			var u refUnsupportedError
			if !errors.As(err, &u) {
				t.Errorf("reference model rejected %q with wrong error layer: %T %v", path, err, err)
			}
		}
	}
	supported := []string{
		"$", "$.a", `$["a"]`, "$[0]", "$[-1]", "$[0,2]", `$["a","b"]`, "$[1:]",
		"$[:-1]", "$[::-2]", "$.*", "$[*]", "$..a", "$..*", `$..["a","b"]`,
		"$.a[0].b", "$.a.*.b",
	}
	for _, path := range supported {
		if _, err := refParse(path); err != nil {
			t.Errorf("reference model rejected supported syntax %q: %v", path, err)
		}
	}
}
