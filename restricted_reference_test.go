package jsonpath_test

// Independent restricted reference interpreter used only by the
// "jsonpath: selector combination, placeholder binding and failure isolation"
// regression story.
//
// The reference deliberately supports only the JSONPath subset the repository
// itself supports in this test matrix and explicitly rejects anything outside
// of that subset. It is a second, independent implementation (its own lexer,
// parser and evaluator): it shares no production algorithm with the package,
// but it mirrors the repository's observable semantics for the supported
// subset so tests can compare values and access locations (paths) produced by
// the public jsonpath entry points.

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

type refStep struct {
	kind   byte // 'd' direct, 'm' multi, 's' slice, '*' star, 'R' recursive descent
	keys   []refKey
	min    int
	max    int
	step   int
	hasMax bool
}

type refKey struct {
	lit string // quoted form, e.g. "a" or numeric text
	num int
	isN bool
}

func parseReferencePath(p string) ([]refStep, error) {
	r := &refReader{s: p}
	if !r.consume('$') {
		return nil, fmt.Errorf("reference: path must start with '$': %q", p)
	}
	var steps []refStep
	for {
		switch r.peek() {
		case 0:
			if len(steps) > 0 && steps[len(steps)-1].kind == 'R' {
				return nil, fmt.Errorf("reference: recursive descent needs a following selector in %q", p)
			}
			return steps, nil
		case '.':
			r.next()
			if r.consume('.') {
				steps = append(steps, refStep{kind: 'R'})
				continue
			}
			if r.peek() == '[' {
				r.next()
				st, err := r.bracket()
				if err != nil {
					return nil, err
				}
				steps = append(steps, st)
				continue
			}
			if r.peek() == '*' {
				r.next()
				steps = append(steps, refStep{kind: '*'})
				continue
			}
			switch r.peek() {
			case 0, '[', '.', '\'', '"':
				return nil, fmt.Errorf("reference: unsupported dot selector in %q", p)
			default:
				id, ok := r.ident()
				if !ok {
					return nil, fmt.Errorf("reference: unsupported member name in %q", p)
				}
				steps = append(steps, refStep{kind: 'd', keys: []refKey{{lit: id}}})
			}
		case '[':
			r.next()
			st, err := r.bracket()
			if err != nil {
				return nil, err
			}
			steps = append(steps, st)
		case '*':
			r.next()
			steps = append(steps, refStep{kind: '*'})
		case '(':
			return nil, fmt.Errorf("reference: script expressions are not supported: %q", p)
		case '?', '\'', '@':
			return nil, fmt.Errorf("reference: unsupported syntax in %q", p)
		default:
			id, ok := r.ident()
			if !ok {
				return nil, fmt.Errorf("reference: unsupported syntax %q near %q", p, r.rest())
			}
			steps = append(steps, refStep{kind: 'd', keys: []refKey{{lit: id}}})
		}
	}
}

type refReader struct {
	s string
	i int
}

func (r *refReader) peek() byte {
	if r.i >= len(r.s) {
		return 0
	}
	return r.s[r.i]
}

func (r *refReader) next() byte {
	c := r.peek()
	if c != 0 {
		r.i++
	}
	return c
}

func (r *refReader) consume(c byte) bool {
	if r.peek() == c {
		r.i++
		return true
	}
	return false
}

func (r *refReader) rest() string {
	if r.i >= len(r.s) {
		return ""
	}
	return r.s[r.i:]
}

func (r *refReader) ident() (string, bool) {
	start := r.i
	first := true
	for r.i < len(r.s) {
		c := r.s[r.i]
		isAlpha := c == '_' || c == '-' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
		isDigit := c >= '0' && c <= '9'
		if isAlpha || (!first && isDigit) {
			r.i++
			first = false
			continue
		}
		break
	}
	return r.s[start:r.i], r.i > start
}

func (r *refReader) integer() (int, bool) {
	start := r.i
	if r.peek() == '-' {
		r.i++
	}
	d := r.i
	for r.i < len(r.s) {
		c := r.s[r.i]
		if c < '0' || c > '9' {
			break
		}
		r.i++
	}
	if r.i == d {
		r.i = start
		return 0, false
	}
	n, err := strconv.Atoi(r.s[start:r.i])
	if err != nil {
		r.i = start
		return 0, false
	}
	return n, true
}

func (r *refReader) bracket() (refStep, error) {
	r.spaces()
	switch r.peek() {
	case '*':
		r.next()
		r.spaces()
		if !r.consume(']') {
			return refStep{}, fmt.Errorf("reference: expected ']' after [*")
		}
		return refStep{kind: '*'}, nil
	case '?':
		return refStep{}, fmt.Errorf("reference: filter expressions are not supported")
	case '\'':
		return refStep{}, fmt.Errorf("reference: single quoted bracket keys are not supported")
	case ']':
		return refStep{}, fmt.Errorf("reference: empty bracket is not supported")
	}

	// decide slice vs key union by looking for ':' before ']'/','
	if n, ok := r.integer(); ok {
		r.spaces()
		if r.peek() == ':' {
			r.next()
			return r.sliceAfterMin(n)
		}
		keys := []refKey{numKey(n)}
		for {
			r.spaces()
			if r.consume(',') {
				r.spaces()
				k, err := r.key()
				if err != nil {
					return refStep{}, err
				}
				keys = append(keys, k)
				continue
			}
			if !r.consume(']') {
				return refStep{}, fmt.Errorf("reference: expected ',' or ']' in bracket, got %q", r.rest())
			}
			if len(keys) == 1 {
				return refStep{kind: 'd', keys: keys}, nil
			}
			return refStep{kind: 'm', keys: keys}, nil
		}
	}
	if r.consume(':') {
		return r.sliceNoMin()
	}
	k, err := r.key()
	if err != nil {
		return refStep{}, err
	}
	keys := []refKey{k}
	for {
		r.spaces()
		if r.consume(',') {
			r.spaces()
			k, err := r.key()
			if err != nil {
				return refStep{}, err
			}
			keys = append(keys, k)
			continue
		}
		if !r.consume(']') {
			return refStep{}, fmt.Errorf("reference: expected ',' or ']' in bracket, got %q", r.rest())
		}
		if len(keys) == 1 {
			return refStep{kind: 'd', keys: keys}, nil
		}
		return refStep{kind: 'm', keys: keys}, nil
	}
}

func (r *refReader) key() (refKey, error) {
	if r.peek() == '"' {
		s, err := r.quoted()
		if err != nil {
			return refKey{}, err
		}
		return refKey{lit: s}, nil
	}
	if n, ok := r.integer(); ok {
		return numKey(n), nil
	}
	return refKey{}, fmt.Errorf("reference: unsupported bracket key at %q", r.rest())
}

func (r *refReader) quoted() (string, error) {
	if !r.consume('"') {
		return "", fmt.Errorf("reference: expected quoted string")
	}
	var sb strings.Builder
	for {
		c := r.next()
		if c == 0 {
			return "", fmt.Errorf("reference: unterminated string")
		}
		if c == '"' {
			return sb.String(), nil
		}
		if c == '\\' {
			c = r.next()
		}
		sb.WriteByte(c)
	}
}

func (r *refReader) spaces() {
	for r.peek() == ' ' || r.peek() == '\t' {
		r.i++
	}
}

func numKey(n int) refKey {
	return refKey{lit: strconv.Itoa(n), num: n, isN: true}
}

func (r *refReader) sliceAfterMin(min int) (refStep, error) {
	// caller has already consumed the ':' separating min and max
	r.spaces()
	return finishSlice(r, min)
}

func (r *refReader) sliceNoMin() (refStep, error) {
	// caller has already consumed the leading ':'
	return finishSlice(r, 0)
}

// finishSlice parses [max][:step]] after the first separator was consumed.
// Missing max/step default to MaxInt32/1 exactly like the repository parser.
func finishSlice(r *refReader, min int) (refStep, error) {
	max := 1<<31 - 1
	hasMax := false
	r.spaces()
	if n, ok := r.integer(); ok {
		max = n
		hasMax = true
	}
	r.spaces()
	step := 1
	hasStepSep := r.consume(':')
	if hasStepSep {
		r.spaces()
		n, ok := r.integer()
		if !ok {
			return refStep{}, fmt.Errorf("reference: invalid slice step at %q", r.rest())
		}
		step = n
		r.spaces()
	}
	if step == 0 {
		step = 1
	}
	if !r.consume(']') {
		return refStep{}, fmt.Errorf("reference: expected ']' at end of slice, got %q", r.rest())
	}
	return refStep{kind: 's', min: min, max: max, step: step, hasMax: hasMax}, nil
}

// ---- reference evaluator ----

type refMatch struct {
	path  []string // access locations from the root, e.g. ["o","b","0"]
	value interface{}
}

func evalReference(root interface{}, steps []refStep) (value interface{}, paths map[string]interface{}, err error) {
	current := []refMatch{{path: []string{}, value: root}}
	ambiguous := false
	for si, st := range steps {
		next := []refMatch{}
		switch st.kind {
		case 'd':
			if !ambiguous {
				m := current[0]
				v, emittedKey, perr := refSelect(m.value, st.keys[0])
				if perr != nil {
					return nil, nil, perr
				}
				current = []refMatch{{path: appendPath(m.path, emittedKey, st.keys[0]), value: v}}
				continue
			}
			for _, m := range current {
				v, emittedKey, perr := refSelect(m.value, st.keys[0])
				if perr != nil {
					continue
				}
				next = append(next, refMatch{path: appendPath(m.path, emittedKey, st.keys[0]), value: v})
			}
			current = next
		case 'm':
			ambiguous = true
			for _, m := range current {
				for _, k := range st.keys {
					v, emittedKey, perr := refSelect(m.value, k)
					if perr != nil {
						continue
					}
					next = append(next, refMatch{path: appendPath(m.path, emittedKey, k), value: v})
				}
			}
			current = next
		case '*':
			ambiguous = true
			for _, m := range current {
				for _, kv := range refChildren(m.value) {
					next = append(next, refMatch{path: appendPath(m.path, kv.key, refKey{lit: kv.key}), value: kv.value})
				}
			}
			current = next
		case 's':
			ambiguous = true
			for _, m := range current {
				o, ok := m.value.([]interface{})
				if !ok {
					continue
				}
				for _, idx := range refSliceIndices(len(o), st) {
					next = append(next, refMatch{
						path:  appendPath(m.path, strconv.Itoa(idx), numKey(idx)),
						value: o[idx],
					})
				}
			}
			current = next
		case 'R':
			ambiguous = true
			follow := steps[si+1].kind
			_ = follow
			for _, m := range current {
				refDescend(m.value, m.path, func(nm refMatch) {
					next = append(next, nm)
				})
			}
			current = next
		}
	}
	if !ambiguous {
		m := current[0]
		return m.value, map[string]interface{}{refRenderPlain(m.path): m.value}, nil
	}
	values := make([]interface{}, 0, len(current))
	paths = map[string]interface{}{}
	for _, m := range current {
		values = append(values, m.value)
		paths[refRenderAmbiguous(m.path)] = m.value
	}
	return values, paths, nil
}

type refChild struct {
	key   string
	value interface{}
}

func refChildren(v interface{}) []refChild {
	switch o := v.(type) {
	case []interface{}:
		out := make([]refChild, len(o))
		for i, e := range o {
			out[i] = refChild{strconv.Itoa(i), e}
		}
		return out
	case map[string]interface{}:
		keys := make([]string, 0, len(o))
		for k := range o {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := make([]refChild, 0, len(keys))
		for _, k := range keys {
			out = append(out, refChild{k, o[k]})
		}
		return out
	}
	return nil
}

// refSelect mirrors selectValue semantics of the repository:
// arrays accept integer keys (negative indexing, out-of-bounds yields a nil
// match without error for bracket/multi selection), objects accept string
// keys (unknown keys are errors), anything else is a type error.
func refSelect(v interface{}, k refKey) (out interface{}, emitted string, err error) {
	switch o := v.(type) {
	case []interface{}:
		if !k.isN {
			return nil, "", fmt.Errorf("could not select value, invalid key: expected number but got %s (string)", k.lit)
		}
		i := k.num
		p := i
		if i < 0 {
			p = len(o) + i
		}
		if p < 0 || p >= len(o) {
			return nil, "", nil
		}
		return o[p], strconv.Itoa(i), nil
	case map[string]interface{}:
		val, ok := o[k.lit]
		if !ok {
			return nil, "", fmt.Errorf("unknown key %s", k.lit)
		}
		return val, k.lit, nil
	default:
		return nil, "", fmt.Errorf("unsupported value type %T for select, expected map[string]interface{}, []interface{} or Array", o)
	}
}

func appendPath(base []string, emitted string, k refKey) []string {
	if emitted == "" {
		return cloneAppend(base, strconv.Itoa(k.num))
	}
	return cloneAppend(base, emitted)
}

func cloneAppend(base []string, extra ...string) []string {
	out := make([]string, 0, len(base)+len(extra))
	out = append(out, base...)
	out = append(out, extra...)
	return out
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

func refSliceIndices(n int, st refStep) []int {
	min := refNegmax(st.min, n)
	max := refNegmax(st.max, n)
	if min > max {
		return nil
	}
	var idx []int
	if st.step > 0 {
		for i := min; i < max; i += st.step {
			idx = append(idx, i)
		}
	} else {
		for i := max - 1; i >= min; i += st.step {
			idx = append(idx, i)
		}
	}
	return idx
}

func refDescend(v interface{}, path []string, emit func(refMatch)) {
	emit(refMatch{path: cloneAppend(path), value: v})
	for _, kv := range refChildren(v) {
		childPath := cloneAppend(path, kv.key)
		refDescend(kv.value, childPath, emit)
	}
}

func refQuoteSegment(s string) string {
	return strconv.Quote(s)
}

// refRenderPlain renders direct-selector path keys. Direct array selection
// keeps the raw requested index, including a negative sign, matching the
// repository (e.g. $[-1] -> $["-1"]).
func refRenderPlain(segs []string) string {
	var sb strings.Builder
	sb.WriteString("$")
	for _, s := range segs {
		sb.WriteString("[")
		sb.WriteString(refQuoteSegment(s))
		sb.WriteString("]")
	}
	return sb.String()
}

// refRenderAmbiguous renders paths produced by wildcard/range/descent, where
// array indices are always the actual visited non-negative index.
func refRenderAmbiguous(segs []string) string {
	return refRenderPlain(segs)
}
