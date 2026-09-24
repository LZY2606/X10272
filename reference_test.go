package jsonpath_test

// reference_test.go contains an independent, deliberately restricted reference
// interpreter for the JSONPath subset that this repository supports. It is used
// by the selector composition contract tests as a cross-check: parser results
// produced through the public jsonpath entry points are compared against this
// interpreter's values and full access locations.
//
// The reference model intentionally rejects every construct outside the
// supported subset (filters, scripts, quoted bracket keys, logical operators,
// functions, ...) instead of silently guessing a meaning.

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// refUnsupported marks a query that is outside the reference model's subset.
var refUnsupported = fmt.Errorf("reference model: syntax outside supported jsonpath subset")

type refOpKind int

const (
	refChild refOpKind = iota
	refStar
	refUnion
	refSlice
	refDescentChild
	refDescentStar
	refDescentUnion
)

type refKey struct {
	isIndex bool
	idx     int
	key     string
}

type refOp struct {
	kind           refOpKind
	isIndex        bool
	idx            int
	key            string
	keys           []refKey
	min, max, step int
}

// refParse turns a JSONPath expression of the supported subset into a list of
// operations. It is independent from the production parser (no reuse of gval
// or of any internal jsonpath type).
func refParse(query string) ([]refOp, error) {
	s := &refScanner{src: query}
	s.skipSpaces()
	if !s.consume('$') {
		return nil, fmt.Errorf("%w: %q (path must start with $)", refUnsupported, query)
	}
	var ops []refOp
	for {
		c := s.peek()
		switch {
		case c == 0:
			return ops, nil
		case c == '.':
			s.next()
			if s.peek() == '.' {
				s.next()
				op, err := s.parseDescentTarget(query)
				if err != nil {
					return nil, err
				}
				ops = append(ops, op)
			} else if s.peek() == '*' {
				s.next()
				ops = append(ops, refOp{kind: refStar})
			} else if isRefIdentStart(s.peek()) {
				name := s.readIdent()
				ops = append(ops, refOp{kind: refChild, isIndex: false, key: name})
			} else {
				return nil, fmt.Errorf("%w: %q (expected identifier, '*' or '.' after dot)", refUnsupported, query)
			}
		case c == '[':
			s.next()
			op, err := s.parseBracket(query)
			if err != nil {
				return nil, err
			}
			ops = append(ops, op)
		default:
			return nil, fmt.Errorf("%w: %q (unexpected trailing syntax)", refUnsupported, query)
		}
	}
}

func (s *refScanner) parseDescentTarget(query string) (refOp, error) {
	switch c := s.peek(); {
	case isRefIdentStart(c):
		return refOp{kind: refDescentChild, isIndex: false, key: s.readIdent()}, nil
	case c == '*':
		s.next()
		return refOp{kind: refDescentStar}, nil
	case c == '[':
		s.next()
		s.skipSpaces()
		switch {
		case s.peek() == '*':
			s.next()
			s.skipSpaces()
			if !s.consume(']') {
				return refOp{}, fmt.Errorf("%w: %q (expected ] after [*)", refUnsupported, query)
			}
			return refOp{kind: refDescentStar}, nil
		case s.peek() == ':':
			return refOp{}, fmt.Errorf("%w: %q (mapper can not be combined with range query)", refUnsupported, query)
		case s.peek() == '?', s.peek() == '(':
			return refOp{}, fmt.Errorf("%w: %q (filters and scripts are outside the supported subset)", refUnsupported, query)
		default:
			keys, sep, err := s.parseBracketItems(query)
			if err != nil {
				return refOp{}, err
			}
			if !s.consume(']') {
				return refOp{}, fmt.Errorf("%w: %q (expected ])", refUnsupported, query)
			}
			if sep == ':' {
				return refOp{}, fmt.Errorf("%w: %q (mapper can not be combined with range query)", refUnsupported, query)
			}
			return refOp{kind: refDescentUnion, keys: keys}, nil
		}
	default:
		return refOp{}, fmt.Errorf("%w: %q (expected identifier, '*' or '[' after '..')", refUnsupported, query)
	}
}

func (s *refScanner) parseBracket(query string) (refOp, error) {
	s.skipSpaces()
	if s.peek() == '*' {
		s.next()
		s.skipSpaces()
		if !s.consume(']') {
			return refOp{}, fmt.Errorf("%w: %q (expected ] after [*)", refUnsupported, query)
		}
		return refOp{kind: refStar}, nil
	}
	if s.peek() == '?' || s.peek() == '(' || s.peek() == '\'' {
		return refOp{}, fmt.Errorf("%w: %q (filters, scripts and single quoted keys are outside the supported subset)", refUnsupported, query)
	}
	keys, sep, err := s.parseBracketItems(query)
	if err != nil {
		return refOp{}, err
	}
	s.skipSpaces()
	if !s.consume(']') {
		return refOp{}, fmt.Errorf("%w: %q (expected ])", refUnsupported, query)
	}
	switch {
	case sep == ':':
		if len(keys) > 3 {
			return refOp{}, fmt.Errorf("%w: %q (range query has at most [min:max:step])", refUnsupported, query)
		}
		defaults := []int{0, math.MaxInt32, 1}
		for len(keys) < 3 {
			keys = append(keys, refKey{isIndex: true, idx: defaults[len(keys)]})
		}
		nums := []int{}
		for _, k := range keys {
			if !k.isIndex {
				return refOp{}, fmt.Errorf("%w: %q (slice bounds must be integers)", refUnsupported, query)
			}
			nums = append(nums, k.idx)
		}
		return refOp{kind: refSlice, min: nums[0], max: nums[1], step: nums[2]}, nil
	case len(keys) == 1:
		return refOp{kind: refChild, isIndex: keys[0].isIndex, idx: keys[0].idx, key: keys[0].key}, nil
	default:
		return refOp{kind: refUnion, keys: keys}, nil
	}
}

// parseBracketItems mirrors the production token accumulation, including the
// implicit bounds inserted at each ':' position.
func (s *refScanner) parseBracketItems(query string) ([]refKey, rune, error) {
	var keys []refKey
	var sep rune
	for {
		s.skipSpaces()
		c := s.peek()
		if c == 0 {
			return nil, 0, fmt.Errorf("%w: %q (unterminated bracket)", refUnsupported, query)
		}
		if c == ']' {
			return keys, sep, nil
		}
		if c == ':' {
			// empty slice part: insert the default bound for this position
			s.next()
			i := 0
			if len(keys) == 1 {
				i = math.MaxInt32
			}
			keys = append(keys, refKey{isIndex: true, idx: i})
			if sep == 0 {
				sep = ':'
			}
			if sep != ':' {
				return nil, 0, fmt.Errorf("%w: %q (mixed %v and : in JSON bracket)", refUnsupported, query, string(sep))
			}
			continue
		}
		switch {
		case c == '"':
			str, err := s.readQuotedString(query)
			if err != nil {
				return nil, 0, err
			}
			keys = append(keys, refKey{isIndex: false, key: str})
		case c == '-' || (c >= '0' && c <= '9'):
			n, err := s.readInt(query)
			if err != nil {
				return nil, 0, err
			}
			keys = append(keys, refKey{isIndex: true, idx: n})
		default:
			return nil, 0, fmt.Errorf("%w: %q (bare identifiers in brackets are outside the supported subset)", refUnsupported, query)
		}
		s.skipSpaces()
		c = s.peek()
		if c == ']' {
			return keys, sep, nil
		}
		if c != ':' && c != ',' {
			return nil, 0, fmt.Errorf("%w: %q (expected ',' or ':' in bracket)", refUnsupported, query)
		}
		if sep == 0 {
			sep = c
		}
		if sep != c {
			return nil, 0, fmt.Errorf("%w: %q (mixed %v and %v in JSON bracket)", refUnsupported, query, string(sep), string(c))
		}
		s.next()
		if c == ':' {
			i := 0
			if len(keys) == 1 {
				i = math.MaxInt32
			}
			keys = append(keys, refKey{isIndex: true, idx: i})
		}
	}
}

type refScanner struct {
	src string
	pos int
}

func (s *refScanner) peek() rune {
	if s.pos >= len(s.src) {
		return 0
	}
	return rune(s.src[s.pos])
}

func (s *refScanner) next() rune {
	c := s.peek()
	if c != 0 {
		s.pos++
	}
	return c
}

func (s *refScanner) consume(want rune) bool {
	if s.peek() == want {
		s.pos++
		return true
	}
	return false
}

func (s *refScanner) skipSpaces() {
	for s.pos < len(s.src) && unicode.IsSpace(rune(s.src[s.pos])) {
		s.pos++
	}
}

func isRefIdentStart(c rune) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isRefIdentPart(c rune) bool {
	return isRefIdentStart(c) || (c >= '0' && c <= '9')
}

func (s *refScanner) readIdent() string {
	start := s.pos
	for s.pos < len(s.src) && isRefIdentPart(rune(s.src[s.pos])) {
		s.pos++
	}
	return s.src[start:s.pos]
}

func (s *refScanner) readInt(query string) (int, error) {
	start := s.pos
	if s.peek() == '-' {
		s.next()
	}
	digits := s.pos
	for s.pos < len(s.src) && s.src[s.pos] >= '0' && s.src[s.pos] <= '9' {
		s.pos++
	}
	if s.pos == digits {
		return 0, fmt.Errorf("%w: %q (expected integer)", refUnsupported, query)
	}
	n, err := strconv.Atoi(s.src[start:s.pos])
	if err != nil {
		return 0, fmt.Errorf("%w: %q (integer out of range)", refUnsupported, query)
	}
	return n, nil
}

func (s *refScanner) readQuotedString(query string) (string, error) {
	s.next() // opening quote
	var sb strings.Builder
	for s.pos < len(s.src) {
		c := s.src[s.pos]
		if c == '\\' {
			s.pos++
			if s.pos >= len(s.src) {
				break
			}
			sb.WriteByte(s.src[s.pos])
			s.pos++
			continue
		}
		if c == '"' {
			s.pos++
			return sb.String(), nil
		}
		sb.WriteByte(c)
		s.pos++
	}
	return "", fmt.Errorf("%w: %q (unterminated string)", refUnsupported, query)
}

func sortRefKeys(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ---------------------------------------------------------------------------
// Evaluation
// ---------------------------------------------------------------------------

type refState struct {
	loc  []string
	wild []string
	val  interface{}
}

func refJoin(base []string, more ...string) []string {
	out := make([]string, 0, len(base)+len(more))
	out = append(out, base...)
	out = append(out, more...)
	return out
}

// refSelect mirrors selector.selectValue for plain JSON containers.
func refSelect(v interface{}, isIndex bool, idx int, key string) (string, interface{}, error) {
	switch o := v.(type) {
	case []interface{}:
		if !isIndex {
			return "", nil, fmt.Errorf("could not select value, invalid key: expected number but got %s (string)", key)
		}
		p := idx
		if idx < 0 {
			p = len(o) + idx
		}
		if p < 0 || p >= len(o) {
			return strconv.Itoa(idx), nil, nil
		}
		return strconv.Itoa(idx), o[p], nil
	case map[string]interface{}:
		k := key
		if isIndex {
			k = strconv.Itoa(idx)
		}
		if r, ok := o[k]; ok {
			return k, r, nil
		}
		return "", nil, fmt.Errorf("unknown key %s", k)
	default:
		return "", nil, fmt.Errorf("unsupported value type %T for select, expected map[string]interface{}, []interface{} or Array", v)
	}
}

// refChildren enumerates children in the reproducible production order:
// array indices ascending, object keys lexicographically sorted.
func refChildren(v interface{}, visit func(key string, child interface{})) {
	switch o := v.(type) {
	case []interface{}:
		for i, child := range o {
			visit(strconv.Itoa(i), child)
		}
	case map[string]interface{}:
		for _, k := range sortRefKeys(o) {
			visit(k, o[k])
		}
	}
}

func refNegMax(n, length int) int {
	if n < 0 {
		n = length + n
		if n < 0 {
			n = 0
		}
	} else if n > length {
		return length
	}
	return n
}

type refHit struct {
	key string
	val interface{}
}

func refSliceHits(v interface{}, min, max, step int) []refHit {
	o, ok := v.([]interface{})
	if !ok {
		return nil
	}
	if step == 0 {
		step = 1
	}
	n := len(o)
	min = refNegMax(min, n)
	max = refNegMax(max, n)
	if min > max {
		return nil
	}
	hits := []refHit{}
	if step > 0 {
		for i := min; i < max; i += step {
			hits = append(hits, refHit{strconv.Itoa(i), o[i]})
		}
	} else {
		for i := max - 1; i >= min; i += step {
			hits = append(hits, refHit{strconv.Itoa(i), o[i]})
		}
	}
	return hits
}

// refEval evaluates ops against a JSON-decoded document and returns matched
// states in production visitation order. Child selector errors are fatal only
// when the whole path is unambiguous; otherwise the failing state is dropped,
// exactly like the production matcher chain.
func refEval(ops []refOp, doc interface{}) ([]refState, error) {
	return refEvalWithSlice(ops, doc, refSliceHits)
}

// refEvalWithSlice is refEval parameterized on the slice semantics, so the
// contract tests can inject deliberately wrong (mutated) slice behaviour and
// prove the comparison harness catches it.
func refEvalWithSlice(ops []refOp, doc interface{}, sliceFn func(v interface{}, min, max, step int) []refHit) ([]refState, error) {
	ambiguous := false
	for _, op := range ops {
		if op.kind != refChild {
			ambiguous = true
		}
	}
	states := []refState{{val: doc}}
	for _, op := range ops {
		next := []refState{}
		for _, st := range states {
			switch op.kind {
			case refChild:
				key, val, err := refSelect(st.val, op.isIndex, op.idx, op.key)
				if err != nil {
					if !ambiguous {
						return nil, err
					}
					continue
				}
				next = append(next, refState{
					loc:  refJoin(st.loc, key),
					wild: st.wild,
					val:  val,
				})
			case refStar:
				refChildren(st.val, func(key string, child interface{}) {
					next = append(next, refState{
						loc:  refJoin(st.loc, key),
						wild: refJoin(st.wild, key),
						val:  child,
					})
				})
			case refUnion:
				for _, k := range op.keys {
					key, val, err := refSelect(st.val, k.isIndex, k.idx, k.key)
					if err != nil {
						continue
					}
					next = append(next, refState{
						loc:  refJoin(st.loc, key),
						wild: refJoin(st.wild, key),
						val:  val,
					})
				}
			case refSlice:
				for _, hit := range sliceFn(st.val, op.min, op.max, op.step) {
					next = append(next, refState{
						loc:  refJoin(st.loc, hit.key),
						wild: refJoin(st.wild, hit.key),
						val:  hit.val,
					})
				}
			case refDescentChild, refDescentStar, refDescentUnion:
				next = append(next, refDescend(st, op)...)
			}
		}
		states = next
	}
	return states, nil
}

// refDescend visits st.val and all of its descendants in mapper order
// (node first, then children recursively) and applies the descent target.
func refDescend(st refState, op refOp) []refState {
	out := []refState{}
	var walk func(base []string, v interface{})
	walk = func(base []string, v interface{}) {
		switch op.kind {
		case refDescentChild:
			key, val, err := refSelect(v, op.isIndex, op.idx, op.key)
			if err == nil {
				out = append(out, refState{
					loc:  refJoin(refJoin(st.loc, base...), key),
					wild: refJoin(st.wild, base...),
					val:  val,
				})
			}
		case refDescentStar:
			refChildren(v, func(key string, child interface{}) {
				out = append(out, refState{
					loc:  refJoin(refJoin(st.loc, base...), key),
					wild: refJoin(refJoin(st.wild, base...), key),
					val:  child,
				})
			})
		case refDescentUnion:
			for _, k := range op.keys {
				key, val, err := refSelect(v, k.isIndex, k.idx, k.key)
				if err != nil {
					continue
				}
				out = append(out, refState{
					loc:  refJoin(refJoin(st.loc, base...), key),
					wild: refJoin(refJoin(st.wild, base...), key),
					val:  val,
				})
			}
		}
		refChildren(v, func(key string, child interface{}) {
			walk(refJoin(base, key), child)
		})
	}
	walk(nil, st.val)
	return out
}

// refLoc renders a full access location exactly like production toJSONPath.
func refLoc(segments []string) string {
	var sb strings.Builder
	sb.WriteString("$")
	for _, seg := range segments {
		sb.WriteString("[")
		sb.WriteString(strconv.Quote(seg))
		sb.WriteString("]")
	}
	return sb.String()
}

// refValue assembles the caller-visible value: a plain result for unambiguous
// paths, a list of matches otherwise.
func refValue(ops []refOp, states []refState) interface{} {
	ambiguous := false
	for _, op := range ops {
		if op.kind != refChild {
			ambiguous = true
		}
	}
	if !ambiguous {
		return states[0].val
	}
	vals := []interface{}{}
	for _, st := range states {
		vals = append(vals, st.val)
	}
	return vals
}

// refPaths assembles the full-path map produced by GetWithPaths.
func refPaths(states []refState) map[string]interface{} {
	m := map[string]interface{}{}
	for _, st := range states {
		m[refLoc(st.loc)] = st.val
	}
	return m
}

// ---------------------------------------------------------------------------
// Placeholder objects {#: path} / {#i: path}
// ---------------------------------------------------------------------------

type refPlaceholder struct {
	all   bool
	idx   int
	inner []refOp
}

// refParsePlaceholder parses a single-entry placeholder object. Multi-entry
// objects and non-placeholder keys are outside the reference subset.
func refParsePlaceholder(query string) (*refPlaceholder, error) {
	trimmed := strings.TrimSpace(query)
	if !strings.HasPrefix(trimmed, "{") || !strings.HasSuffix(trimmed, "}") {
		return nil, fmt.Errorf("%w: %q (placeholder query must be a single object)", refUnsupported, query)
	}
	body := strings.TrimSpace(trimmed[1 : len(trimmed)-1])
	if !strings.HasPrefix(body, "#") {
		return nil, fmt.Errorf("%w: %q (only placeholder keys are supported)", refUnsupported, query)
	}
	body = body[1:]
	ph := &refPlaceholder{all: true}
	i := 0
	for i < len(body) && body[i] >= '0' && body[i] <= '9' {
		i++
	}
	if i > 0 {
		n, err := strconv.Atoi(body[:i])
		if err != nil {
			return nil, fmt.Errorf("%w: %q (bad placeholder index)", refUnsupported, query)
		}
		ph.all = false
		ph.idx = n
		body = body[i:]
	}
	body = strings.TrimSpace(body)
	if !strings.HasPrefix(body, ":") {
		return nil, fmt.Errorf("%w: %q (expected ':' after placeholder)", refUnsupported, query)
	}
	inner := strings.TrimSpace(body[1:])
	ops, err := refParse(inner)
	if err != nil {
		return nil, err
	}
	ph.inner = ops
	return ph, nil
}

// refEvalPlaceholder evaluates a placeholder object into a string map,
// mirroring the production keyValueMatcher semantics.
func refEvalPlaceholder(ph *refPlaceholder, doc interface{}) (map[string]interface{}, error) {
	states, err := refEval(ph.inner, doc)
	if err != nil {
		return nil, err
	}
	out := map[string]interface{}{}
	for _, st := range states {
		var key string
		if ph.all {
			key = refLoc(st.wild)
		} else {
			if len(st.wild) <= ph.idx {
				return nil, fmt.Errorf("JSONPath placeholder #%d is not available", ph.idx)
			}
			key = st.wild[ph.idx]
		}
		out[key] = st.val
	}
	return out, nil
}
