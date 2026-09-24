package jsonpath

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// This file contains the independent restricted reference interpreter used by
// the selector-combination / placeholder-binding / failure-isolation tests.
//
// The reference interpreter does not parse JSONPath itself: it receives the
// segments produced by the production parser (parseSegments, which captures
// the path built by newParser/parser.parse) and evaluates them with its own
// deliberately small evaluation code. It only covers the subset that the
// contract tests exercise:
//
//	directSelector with a constant key  ($.x, $[1], $[-1])
//	starSelector                        ($.x.*, $[*])
//	multiSelector with constant keys    ($[1, 3])
//	rangeSelector with constant bounds  ($[1:3], $[-2:], $[::-1])
//	mapperSelector (recursive descent)  ($..x, $..*)
//
// Every other selector the production parser can build (current element "@",
// filters "[?...]", scripts "(...)", non-constant keys) is rejected with
// errReferenceUnsupported, so the model's subset boundary is explicit instead
// of silently drifting.

var errReferenceUnsupported = fmt.Errorf("restricted reference interpreter: unsupported selector segment")

type refMatch struct {
	keys []interface{}
	v    interface{}
}

type refOutcome struct {
	matches []refMatch
	ambiguous bool
	err     error
}

// refPolicy lets the mutation tests run the same independent interpreter with
// a single deliberately injected endpoint mutation. Production code is never
// mutated; only this local policy is.
type refPolicy struct {
	name string
	// normStart/negIndex alter slice boundary and negative-index arithmetic.
	normStart func(i, n int) int
	normEnd   func(i, n int) int
	negIndex  func(i, n int) int
}

func defaultRefPolicy() refPolicy {
	return refPolicy{
		name: "correct",
		normStart: func(i, n int) int {
			if i < 0 {
				i += n
				if i < 0 {
					i = 0
				}
			}
			if i > n {
				i = n
			}
			return i
		},
		normEnd: func(i, n int) int {
			if i < 0 {
				i += n
				if i < 0 {
					i = 0
				}
			}
			if i > n {
				i = n
			}
			return i
		},
		negIndex: func(i, n int) int { return n + i },
	}
}

func evalConstInt(e interface{ EvalInt(context.Context, interface{}) (int, error) }, root interface{}) (int, bool) {
	i, err := e.EvalInt(context.Background(), root)
	if err != nil {
		return 0, false
	}
	return i, true
}

func evalConstString(e interface{ EvalString(context.Context, interface{}) (string, error) }, root interface{}) (string, bool) {
	s, err := e.EvalString(context.Background(), root)
	if err != nil {
		return "", false
	}
	return s, true
}

// referenceEvaluate runs the parsed segments with the independent interpreter.
// budget < 0 means unlimited recursive-descent visits; ctx is observed for
// cancellation exactly like the production path.
func referenceEvaluate(segments []pathSegment, root interface{}, budget int, ctx context.Context, pol refPolicy) refOutcome {
	if ctx == nil {
		ctx = context.Background()
	}
	state := &refState{root: root, budget: budget, ctx: ctx, pol: pol}
	current := []refMatch{{keys: []interface{}{}, v: root}}
	ambiguous := false
	for _, seg := range segments {
		next := make([]refMatch, 0, len(current))
		for _, m := range current {
			if seg.isAmbiguous() {
				ambiguous = true
				next = state.expandAmbiguous(seg.ambiguous, m, next)
			} else {
				k, v, err := state.selectPlain(seg.plain, m.v)
				if err != nil {
					if !ambiguous {
						return refOutcome{ambiguous: false, err: err}
					}
					continue
				}
				keys := m.keys
				if k != nil {
					keys = append(append([]interface{}{}, keys...), k)
				}
				next = append(next, refMatch{keys: keys, v: v})
			}
		}
		if state.err != nil {
			return refOutcome{matches: next, ambiguous: ambiguous, err: state.err}
		}
		current = next
	}
	return refOutcome{matches: current, ambiguous: ambiguous, err: nil}
}

type refState struct {
	root   interface{}
	budget int
	visits int
	ctx    context.Context
	pol    refPolicy
	err    error
}

func (s *refState) selectPlain(sel plainSelector, v interface{}) (interface{}, interface{}, error) {
	switch sel := sel.(type) {
	case directSelector:
		return s.direct(sel.key, v)
	default:
		return nil, nil, errReferenceUnsupported
	}
}

func (s *refState) direct(key interface {
	EvalInt(context.Context, interface{}) (int, error)
	EvalString(context.Context, interface{}) (string, error)
}, v interface{}) (interface{}, interface{}, error) {
	switch o := v.(type) {
	case []interface{}:
		i, ok := evalConstInt(key, s.root)
		if !ok {
			return nil, nil, errReferenceUnsupported
		}
		p := s.pol.negIndex(i, len(o))
		if i >= 0 {
			p = i
		}
		if p < 0 || p >= len(o) {
			return nil, strconv.Itoa(i), nil
		}
		return o[p], strconv.Itoa(i), nil
	case map[string]interface{}:
		k, ok := evalConstString(key, s.root)
		if !ok {
			return nil, nil, errReferenceUnsupported
		}
		r, ok := o[k]
		if !ok {
			return nil, "", fmt.Errorf("unknown key %s", k)
		}
		return r, k, nil
	default:
		return nil, "", fmt.Errorf("unsupported value type %T for select, expected map[string]interface{}, []interface{} or Array", o)
	}
}

func (s *refState) expandAmbiguous(sel ambiguousSelector, m refMatch, out []refMatch) []refMatch {
	switch sel := sel.(type) {
	case starSelector:
		return s.eachChild(m.v, func(k string, v interface{}) {
			out = append(out, refMatch{keys: append(append([]interface{}{}, m.keys...), k), v: v})
		}, out)
	case multiSelector:
		for _, k := range sel.keys {
			_, v, err := s.direct(k, m.v)
			if err != nil {
				continue
			}
			wildcard := ""
			if i, ok := evalConstInt(k, s.root); ok {
				wildcard = strconv.Itoa(i)
			} else if name, ok := evalConstString(k, s.root); ok {
				wildcard = name
			}
			out = append(out, refMatch{keys: append(append([]interface{}{}, m.keys...), wildcard), v: v})
		}
		return out
	case rangeSelector:
		o, ok := m.v.([]interface{})
		if !ok {
			return out
		}
		min, ok1 := evalConstInt(sel.min, s.root)
		max, ok2 := evalConstInt(sel.max, s.root)
		step, ok3 := evalConstInt(sel.step, s.root)
		if !ok1 || !ok2 || !ok3 {
			s.err = errReferenceUnsupported
			return out
		}
		if step == 0 {
			step = 1
		}
		n := len(o)
		min = s.pol.normStart(min, n)
		max = s.pol.normEnd(max, n)
		if min > max {
			return out
		}
		if step > 0 {
			for i := min; i < max; i += step {
				out = append(out, refMatch{keys: append(append([]interface{}{}, m.keys...), strconv.Itoa(i)), v: o[i]})
			}
		} else {
			for i := max - 1; i >= min; i += step {
				out = append(out, refMatch{keys: append(append([]interface{}{}, m.keys...), strconv.Itoa(i)), v: o[i]})
			}
		}
		return out
	case mapperSelector:
		var walk func(keys []interface{}, v interface{})
		walk = func(keys []interface{}, v interface{}) {
			if s.err != nil {
				return
			}
			if err := s.ctx.Err(); err != nil {
				s.err = err
				return
			}
			if s.budget >= 0 && s.visits >= s.budget {
				s.err = fmt.Errorf("%w (limit %d)", ErrVisitBudgetExceeded, s.budget)
				return
			}
			s.visits++
			out = append(out, refMatch{keys: append([]interface{}{}, keys...), v: v})
			s.eachChild(v, func(k string, child interface{}) {
				walk(append(append([]interface{}{}, keys...), k), child)
			}, nil)
		}
		walk(m.keys, m.v)
		return out
	default:
		s.err = errReferenceUnsupported
		return out
	}
}

// eachChild visits array elements by index and object members in lexicographic
// key order (mirroring visitAll's reproducible order).
func (s *refState) eachChild(v interface{}, visit func(k string, v interface{}), out []refMatch) []refMatch {
	switch o := v.(type) {
	case []interface{}:
		for i, e := range o {
			visit(strconv.Itoa(i), e)
		}
	case map[string]interface{}:
		keys := make([]string, 0, len(o))
		for k := range o {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			visit(k, o[k])
		}
	}
	return out
}

// refValue is the observable value of an outcome, matching the production
// distinction between a plain result and an ambiguous match list.
func (o refOutcome) refValue() interface{} {
	if o.err != nil {
		return nil
	}
	if !o.ambiguous {
		if len(o.matches) == 0 {
			return nil
		}
		return o.matches[len(o.matches)-1].v
	}
	values := make([]interface{}, 0, len(o.matches))
	for _, m := range o.matches {
		values = append(values, m.v)
	}
	return values
}

func (o refOutcome) refPaths() map[string]interface{} {
	m := map[string]interface{}{}
	if o.err != nil {
		return m
	}
	for _, match := range o.matches {
		m[toJSONPath(match.keys)] = match.v
	}
	return m
}

// compareResult describes the first value/path divergence, or "" on equality.
// It intentionally carries the access position and surrounding context instead
// of returning a plain bool.
type refDiff struct {
	where string
	detail string
}

func (d refDiff) String() string {
	if d.where == "" {
		return ""
	}
	return fmt.Sprintf("first divergence at %s: %s", d.where, d.detail)
}

func diffMaps(want, got map[string]interface{}) refDiff {
	for k, wv := range want {
		gv, ok := got[k]
		if !ok {
			return refDiff{where: "access position " + k, detail: fmt.Sprintf("reference visits position but implementation does not; reference value %#v", wv)}
		}
		if !valuesEqual(wv, gv) {
			return refDiff{where: "access position " + k, detail: fmt.Sprintf("reference value %#v, implementation value %#v", wv, gv)}
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			return refDiff{where: "access position " + k, detail: fmt.Sprintf("implementation visits an extra position with value %#v", got[k])}
		}
	}
	return refDiff{}
}

func valuesEqual(a, b interface{}) bool {
	if am, ok := a.(map[string]interface{}); ok {
		bm, ok := b.(map[string]interface{})
		if !ok || len(am) != len(bm) {
			return false
		}
		for k := range am {
			if !valuesEqual(am[k], bm[k]) {
				return false
			}
		}
		return true
	}
	if as, ok := a.([]interface{}); ok {
		bs, ok := b.([]interface{})
		if !ok || len(as) != len(bs) {
			return false
		}
		for i := range as {
			if !valuesEqual(as[i], bs[i]) {
				return false
			}
		}
		return true
	}
	return a == b
}

func describeErr(err error) string {
	if err == nil {
		return "<nil>"
	}
	return strings.ReplaceAll(err.Error(), "\n", " ")
}
