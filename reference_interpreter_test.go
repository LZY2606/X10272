package jsonpath_test

// This file contains refPath, an independent restricted reference
// interpreter used only by the selector-combination / placeholder-binding /
// failure-isolation regression suite.
//
// It deliberately implements *only* the JSONPath subset the repository
// supports without extra gval modules: root $, current @, dot and bracket
// members, integer indices including negatives, [a,b,...] unions,
// [min:max:step] slices, * wildcard and .. recursive descent. Everything
// else (single-quoted names, filters ?..., scripts (...), brace objects,
// placeholders #, functions, =~ / in / ! operators, ...) is rejected
// explicitly with errRefUnsupported, and malformed subset syntax is rejected
// with errRefSyntax. The walker shares no algorithm or helper with the
// production package: it is a from-scratch model used to cross-check
// observable values and visit locations reached exclusively through the
// public jsonpath.New / jsonpath.Get / jsonpath.GetWithPaths entries.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"

	"github.com/PaesslerAG/jsonpath"
)

var (
	errRefUnsupported = errors.New("reference interpreter: syntax outside the repository-supported subset is explicitly rejected")
	errRefSyntax      = errors.New("reference interpreter: malformed path")
)

type refKey struct {
	isIndex bool
	index   int
	name    string
}

type refStepKind int

const (
	refMember refStepKind = iota
	refUnion
	refSlice
	refStar
	refDescend
)

type refStep struct {
	kind     refStepKind
	key      refKey
	keys     []refKey
	min, max int
	step     int
	sub      *refStep
}

type refPath struct {
	current bool
	steps   []refStep
}

// ---------------- parser ----------------

type refParser struct {
	s       string
	i       int
	current bool
	steps   []refStep
}

func parseRefPath(query string) (*refPath, error) {
	p := &refParser{s: query}
	if len(query) == 0 {
		return nil, fmt.Errorf("%w: empty path", errRefUnsupported)
	}
	switch p.peek() {
	case '$':
		p.advance()
	case '@':
		p.advance()
		p.current = true
	default:
		return nil, fmt.Errorf("%w: path must start with $ or @", errRefUnsupported)
	}
	for {
		c, ok := p.peekOk()
		if !ok {
			break
		}
		switch c {
		case '.':
			p.advance()
			c2, ok2 := p.peekOk()
			if !ok2 {
				return nil, fmt.Errorf("%w: trailing dot", errRefSyntax)
			}
			if c2 == '.' {
				p.advance()
				sub, err := p.readDescendantTail()
				if err != nil {
					return nil, err
				}
				p.steps = append(p.steps, refStep{kind: refDescend, sub: sub})
				continue
			}
			if c2 == '*' {
				p.advance()
				p.steps = append(p.steps, refStep{kind: refStar})
				continue
			}
			name, err := p.readIdent()
			if err != nil {
				return nil, err
			}
			p.steps = append(p.steps, refStep{kind: refMember, key: refKey{name: name}})
		case '[':
			step, err := p.readBracket()
			if err != nil {
				return nil, err
			}
			p.steps = append(p.steps, step)
		default:
			return nil, fmt.Errorf("%w: filters, scripts, operators and brace objects are not in the covered subset", errRefUnsupported)
		}
	}
	return &refPath{current: p.current, steps: p.steps}, nil
}

func (p *refParser) skipSpace() {
	for p.i < len(p.s) && (p.s[p.i] == ' ' || p.s[p.i] == '\t') {
		p.i++
	}
}

func (p *refParser) peek() rune {
	c, _ := p.peekOk()
	return c
}

func (p *refParser) peekOk() (rune, bool) {
	p.skipSpace()
	if p.i >= len(p.s) {
		return 0, false
	}
	return rune(p.s[p.i]), true
}

func (p *refParser) advance() rune {
	c, ok := p.peekOk()
	if !ok {
		return 0
	}
	p.i++
	return c
}

func isRefIdentByte(c byte) bool {
	return c == '_' || c == '-' ||
		(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
		(c >= '0' && c <= '9')
}

func (p *refParser) readIdent() (string, error) {
	start := p.i
	for p.i < len(p.s) && isRefIdentByte(p.s[p.i]) {
		if p.s[p.i] == '-' && p.i == start {
			break
		}
		p.i++
	}
	if start == p.i {
		return "", fmt.Errorf("%w: expected identifier at offset %d", errRefUnsupported, p.i)
	}
	return p.s[start:p.i], nil
}

func (p *refParser) readDescendantTail() (*refStep, error) {
	c, ok := p.peekOk()
	if !ok {
		return nil, fmt.Errorf("%w: recursive descent needs a selector", errRefSyntax)
	}
	if c == '*' {
		p.advance()
		return &refStep{kind: refStar}, nil
	}
	if c == '[' {
		step, err := p.readBracket()
		if err != nil {
			return nil, err
		}
		if step.kind == refSlice {
			return nil, fmt.Errorf("%w: mapper can not be combined with range query", errRefUnsupported)
		}
		return &step, nil
	}
	if c == '.' {
		return nil, fmt.Errorf("%w: triple of dots", errRefUnsupported)
	}
	name, err := p.readIdent()
	if err != nil {
		return nil, err
	}
	return &refStep{kind: refMember, key: refKey{name: name}}, nil
}

func (p *refParser) readString() (string, error) {
	start := p.i
	p.i++ // opening quote
	for p.i < len(p.s) {
		if p.s[p.i] == '\\' {
			p.i += 2
			continue
		}
		if p.s[p.i] == '"' {
			q := p.s[start : p.i+1]
			p.i++
			return strconv.Unquote(q)
		}
		p.i++
	}
	return "", fmt.Errorf("%w: unterminated string", errRefSyntax)
}

func (p *refParser) readSignedInt() (int, error) {
	start := p.i
	if p.i < len(p.s) && p.s[p.i] == '-' {
		p.i++
	}
	digits := p.i
	for p.i < len(p.s) && p.s[p.i] >= '0' && p.s[p.i] <= '9' {
		p.i++
	}
	if digits == p.i {
		return 0, fmt.Errorf("%w: expected integer", errRefUnsupported)
	}
	n, err := strconv.Atoi(p.s[start:p.i])
	if err != nil {
		return 0, fmt.Errorf("%w: invalid integer: %v", errRefSyntax, err)
	}
	return n, nil
}

func (p *refParser) readBracket() (refStep, error) {
	p.advance() // [
	var keys []refKey
	hasColon := false
	colonCount := 0
	var bounds [3]*int
	gotOne := false
	for {
		c, ok := p.peekOk()
		if !ok {
			return refStep{}, fmt.Errorf("%w: unclosed bracket", errRefSyntax)
		}
		switch c {
		case ']':
			p.advance()
			if hasColon || colonCount > 0 {
				defs := [3]int{0, math.MaxInt32, 1}
				out := refStep{kind: refSlice}
				for i, b := range bounds {
					if b == nil {
						out.setBound(i, defs[i])
					} else {
						out.setBound(i, *b)
					}
				}
				return out, nil
			}
			if len(keys) == 0 {
				return refStep{}, fmt.Errorf("%w: empty bracket", errRefSyntax)
			}
			if len(keys) == 1 {
				return refStep{kind: refMember, key: keys[0]}, nil
			}
			return refStep{kind: refUnion, keys: keys}, nil
		case ':':
			if len(keys) > 0 {
				if colonCount > 0 || len(keys) > 1 {
					return refStep{}, fmt.Errorf("%w: mixed union and slice in bracket", errRefSyntax)
				}
				first := keys[0]
				if !first.isIndex {
					return refStep{}, fmt.Errorf("%w: string slice bound", errRefUnsupported)
				}
				n := first.index
				bounds[0] = &n
				keys = nil
			}
			colonCount++
			if colonCount > 2 {
				return refStep{}, fmt.Errorf("%w: range query allows at most [min:max:step]", errRefSyntax)
			}
			p.advance()
		case ',':
			if hasColon || colonCount > 0 {
				return refStep{}, fmt.Errorf("%w: mixed slice and union in bracket", errRefSyntax)
			}
			p.advance()
		case '*':
			p.advance()
			if p.advance() != ']' {
				return refStep{}, fmt.Errorf("%w: expected ] after [*", errRefSyntax)
			}
			if gotOne || len(keys) > 0 {
				return refStep{}, fmt.Errorf("%w: wildcard mixed with other keys", errRefSyntax)
			}
			return refStep{kind: refStar}, nil
		case '"':
			s, err := p.readString()
			if err != nil {
				return refStep{}, err
			}
			if hasColon || colonCount > 0 {
				return refStep{}, fmt.Errorf("%w: string slice bound", errRefUnsupported)
			}
			keys = append(keys, refKey{name: s})
			gotOne = true
		case '\'':
			return refStep{}, fmt.Errorf("%w: single-quoted member", errRefUnsupported)
		case '?':
			return refStep{}, fmt.Errorf("%w: filter expression", errRefUnsupported)
		case '(':
			return refStep{}, fmt.Errorf("%w: script expression", errRefUnsupported)
		case '{':
			return refStep{}, fmt.Errorf("%w: object in bracket", errRefUnsupported)
		default:
			if c == '-' || (c >= '0' && c <= '9') {
				n, err := p.readSignedInt()
				if err != nil {
					return refStep{}, err
				}
				if hasColon || colonCount > 0 {
					if bounds[colonCount] != nil {
						return refStep{}, fmt.Errorf("%w: two bounds in one slice slot", errRefSyntax)
					}
					bounds[colonCount] = &n
				} else {
					keys = append(keys, refKey{isIndex: true, index: n})
				}
				gotOne = true
			} else {
				return refStep{}, fmt.Errorf("%w: non-integer/non-string bracket key", errRefUnsupported)
			}
		}
	}
}

func (s *refStep) setBound(i, v int) {
	switch i {
	case 0:
		s.min = v
	case 1:
		s.max = v
	case 2:
		s.step = v
	}
}

// ---------------- evaluator ----------------

type refMatch struct {
	trail []interface{}
	value interface{}
}

type refWalker struct {
	ctx    context.Context
	budget int // <0 unlimited, 0 exhausted
}

func newRefWalker(ctx context.Context) *refWalker {
	budget := -1
	if n, ok := ctx.Value(jsonpath.VisitBudgetContextKey{}).(int); ok && n > 0 {
		budget = n
	}
	return &refWalker{ctx: ctx, budget: budget}
}

func (w *refWalker) visitNode() error {
	if err := w.ctx.Err(); err != nil {
		return err
	}
	if w.budget == 0 {
		return jsonpath.ErrVisitBudgetExceeded
	}
	if w.budget > 0 {
		w.budget--
	}
	return nil
}

func refOrderedKeys(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		ai, aerr := strconv.Atoi(a)
		bi, berr := strconv.Atoi(b)
		if aerr == nil && berr == nil {
			return ai < bi
		}
		return a < b
	})
	return keys
}

func refAppendTrail(trail []interface{}, k interface{}) []interface{} {
	t := make([]interface{}, len(trail)+1)
	copy(t, trail)
	t[len(trail)] = k
	return t
}

func refLocation(trail []interface{}) string {
	out := "$"
	for _, seg := range trail {
		out += "[" + strconv.Quote(fmt.Sprint(seg)) + "]"
	}
	return out
}

// refSelectChild mirrors production selectValue semantics for the supported
// data types: negative indices count from the end, out-of-range indices emit
// nil carrying the raw index key, missing object keys and type mismatches
// error.
func refSelectChild(v interface{}, k refKey) (interface{}, interface{}, error) {
	switch o := v.(type) {
	case []interface{}:
		if !k.isIndex {
			return nil, nil, fmt.Errorf("could not select value, invalid key: expected number but got %s (string)", k.name)
		}
		p := k.index
		if p < 0 {
			p = len(o) + p
		}
		raw := strconv.Itoa(k.index)
		if p < 0 || p >= len(o) {
			return nil, raw, nil
		}
		return o[p], raw, nil
	case map[string]interface{}:
		name := k.name
		if k.isIndex {
			name = strconv.Itoa(k.index)
		}
		if val, ok := o[name]; ok {
			return val, name, nil
		}
		return nil, nil, fmt.Errorf("unknown key %s", name)
	default:
		return nil, nil, fmt.Errorf("unsupported value type %T for select, expected map[string]interface{}, []interface{} or Array", v)
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

func refApplyChild(m refMatch, k refKey) ([]refMatch, error) {
	val, raw, err := refSelectChild(m.value, k)
	if err != nil {
		return nil, err
	}
	return []refMatch{{trail: refAppendTrail(m.trail, raw), value: val}}, nil
}

func refApplyUnion(m refMatch, keys []refKey) []refMatch {
	out := []refMatch{}
	for _, k := range keys {
		val, raw, err := refSelectChild(m.value, k)
		if err != nil {
			continue
		}
		out = append(out, refMatch{trail: refAppendTrail(m.trail, raw), value: val})
	}
	return out
}

func refApplyWildcard(m refMatch) []refMatch {
	out := []refMatch{}
	switch o := m.value.(type) {
	case []interface{}:
		for i, e := range o {
			out = append(out, refMatch{trail: refAppendTrail(m.trail, strconv.Itoa(i)), value: e})
		}
	case map[string]interface{}:
		for _, k := range refOrderedKeys(o) {
			out = append(out, refMatch{trail: refAppendTrail(m.trail, k), value: o[k]})
		}
	}
	return out
}

func refApplySlice(m refMatch, minV, maxV, stepV int) []refMatch {
	o, ok := m.value.([]interface{})
	if !ok {
		return nil
	}
	if stepV == 0 {
		stepV = 1
	}
	n := len(o)
	minV = refNegmax(minV, n)
	maxV = refNegmax(maxV, n)
	if minV > maxV {
		return nil
	}
	out := []refMatch{}
	if stepV > 0 {
		for i := minV; i < maxV; i += stepV {
			out = append(out, refMatch{trail: refAppendTrail(m.trail, strconv.Itoa(i)), value: o[i]})
		}
	} else {
		for i := maxV - 1; i >= minV; i += stepV {
			out = append(out, refMatch{trail: refAppendTrail(m.trail, strconv.Itoa(i)), value: o[i]})
		}
	}
	return out
}

// refWalk visits v and all its descendants pre-order, honoring cancellation
// and the visit budget exactly like the production mapper does.
func (w *refWalker) refWalk(trail []interface{}, v interface{}, visit func([]interface{}, interface{}) error) error {
	if err := w.visitNode(); err != nil {
		return err
	}
	if err := visit(trail, v); err != nil {
		return err
	}
	switch o := v.(type) {
	case map[string]interface{}:
		for _, k := range refOrderedKeys(o) {
			if err := w.refWalk(refAppendTrail(trail, k), o[k], visit); err != nil {
				return err
			}
		}
	case []interface{}:
		for i, e := range o {
			if err := w.refWalk(refAppendTrail(trail, strconv.Itoa(i)), e, visit); err != nil {
				return err
			}
		}
	}
	return nil
}

func (w *refWalker) refApplyDescend(m refMatch, sub *refStep) ([]refMatch, error) {
	out := []refMatch{}
	err := w.refWalk(m.trail, m.value, func(trail []interface{}, v interface{}) error {
		node := refMatch{trail: trail, value: v}
		switch sub.kind {
		case refMember:
			ms, err := refApplyChild(node, sub.key)
			if err != nil {
				return nil // per-node selection mismatch: skip
			}
			out = append(out, ms...)
		case refUnion:
			out = append(out, refApplyUnion(node, sub.keys)...)
		case refStar:
			out = append(out, refApplyWildcard(node)...)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func isRefFatal(err error) bool {
	return errors.Is(err, jsonpath.ErrVisitBudgetExceeded) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded)
}

func (w *refWalker) evalSteps(steps []refStep, doc interface{}) ([]refMatch, error) {
	matches := []refMatch{{trail: []interface{}{}, value: doc}}
	for _, s := range steps {
		next := []refMatch{}
		for _, m := range matches {
			var ms []refMatch
			var err error
			switch s.kind {
			case refMember:
				ms, err = refApplyChild(m, s.key)
			case refUnion:
				ms = refApplyUnion(m, s.keys)
			case refSlice:
				ms = refApplySlice(m, s.min, s.max, s.step)
			case refStar:
				ms = refApplyWildcard(m)
			case refDescend:
				ms, err = w.refApplyDescend(m, s.sub)
			}
			if err != nil {
				if isRefFatal(err) {
					return nil, err
				}
				continue
			}
			next = append(next, ms...)
		}
		matches = next
	}
	return matches, nil
}

// refResult is the reference model of one evaluation: the plain value (or
// the ordered match list for ambiguous paths) plus the full location map.
type refResult struct {
	value interface{}
	locs  map[string]interface{}
}

// refRun parses and evaluates query against doc with the reference model.
// Parse failures are returned as *refParseError to keep the parse/eval
// error layers distinguishable, mirroring New-then-Eval in production.
type refParseError struct{ err error }

func (e *refParseError) Error() string { return e.err.Error() }
func (e *refParseError) Unwrap() error { return e.err }

func refRun(ctx context.Context, query string, doc interface{}) (refResult, error) {
	rp, err := parseRefPath(query)
	if err != nil {
		return refResult{}, &refParseError{err: err}
	}
	if rp.current {
		return refResult{}, fmt.Errorf("%w: @ current element outside filters", errRefUnsupported)
	}
	plain := true
	for _, s := range rp.steps {
		if s.kind != refMember {
			plain = false
			break
		}
	}
	if plain {
		trail := []interface{}{}
		v := doc
		for _, s := range rp.steps {
			val, raw, err := refSelectChild(v, s.key)
			if err != nil {
				return refResult{}, err
			}
			trail = append(trail, raw)
			v = val
		}
		return refResult{value: v, locs: map[string]interface{}{refLocation(trail): v}}, nil
	}
	w := newRefWalker(ctx)
	matches, err := w.evalSteps(rp.steps, doc)
	if err != nil {
		return refResult{}, err
	}
	values := []interface{}{}
	locs := map[string]interface{}{}
	for _, m := range matches {
		values = append(values, m.value)
		locs[refLocation(m.trail)] = m.value
	}
	return refResult{value: values, locs: locs}, nil
}
