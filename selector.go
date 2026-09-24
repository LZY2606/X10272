package jsonpath

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"

	"github.com/PaesslerAG/gval"
)

// plainSelector evaluate exactly one result
type plainSelector interface {
	selectPlain(c context.Context, r, v interface{}) (interface{}, interface{}, error)
}

// ambiguousSelector evaluate wildcard
type ambiguousSelector interface {
	selectAmbiguous(c context.Context, r, v interface{}, match ambiguousMatcher)
}

// @
type currentElementSelector struct{}

func (currentElementSelector) selectPlain(c context.Context, r, v interface{}) (interface{}, interface{}, error) {
	return nil, c.Value(currentElement{}), nil
}

type currentElement struct{}

func currentContext(c context.Context, v interface{}) context.Context {
	return context.WithValue(c, currentElement{}, v)
}

// .x, [x]
type directSelector struct {
	key gval.Evaluable
}

func (d directSelector) selectPlain(c context.Context, r, v interface{}) (interface{}, interface{}, error) {
	e, k, err := selectValue(c, d.key, r, v)
	if err != nil {
		return nil, nil, err
	}
	return k, e, nil
}

// * / [*]
type starSelector struct{}

func (starSelector) selectAmbiguous(c context.Context, r, v interface{}, match ambiguousMatcher) {
	visitAll(v, func(key string, val interface{}) { match(key, val) })
}

// [x, ...]
type multiSelector struct {
	keys []gval.Evaluable
}

func (m multiSelector) selectAmbiguous(c context.Context, r, v interface{}, match ambiguousMatcher) {
	for _, k := range m.keys {
		e, wildcard, err := selectValue(c, k, r, v)
		if err != nil {
			continue
		}
		match(wildcard, e)
	}
}

func selectValue(c context.Context, key gval.Evaluable, r, v interface{}) (value interface{}, jkey string, err error) {

	c = currentContext(c, v)

	switch o := v.(type) {

	case []interface{}:
		i, err := key.EvalInt(c, r)
		if err != nil {
			return nil, "", fmt.Errorf("could not select value, invalid key: %s", err)
		}
		p := i
		if i < 0 {
			p = len(o) + i
		}
		if p < 0 || p >= len(o) {
			return nil, strconv.Itoa(i), nil
		}
		return o[p], strconv.Itoa(i), nil

	case map[string]interface{}:
		k, err := key.EvalString(c, r)
		if err != nil {
			return nil, "", fmt.Errorf("could not select value, invalid key: %s", err)
		}

		if r, ok := o[k]; ok {
			return r, k, nil
		}
		return nil, "", fmt.Errorf("unknown key %s", k)

	case Array:
		i, err := key.EvalInt(c, r)
		if err != nil {
			return nil, "", fmt.Errorf("could not select value, invalid key: %s", err)
		}
		p := i
		if i < 0 {
			p = o.Len() + i
		}
		if p < 0 || p >= o.Len() {
			return nil, strconv.Itoa(i), nil
		}
		r, err := o.SelectGVal(c, strconv.Itoa(p))
		if err != nil {
			return nil, "", err
		}
		return r, strconv.Itoa(i), nil

	case Object:
		k, err := key.EvalString(c, r)
		if err != nil {
			return nil, "", fmt.Errorf("could not select value, invalid key: %s", err)
		}

		r, err := o.SelectGVal(c, k)
		if err != nil {
			return nil, "", err
		}
		return r, k, nil

	default:
		return nil, "", fmt.Errorf("unsupported value type %T for select, expected map[string]interface{}, []interface{} or Array", o)
	}
}

// ..
type mapperSelector struct{}

func (mapperSelector) selectAmbiguous(c context.Context, r, v interface{}, match ambiguousMatcher) {
	mapper(c, r, v, match)
}

func mapper(c context.Context, r, v interface{}, match ambiguousMatcher) {
	if vb, ok := c.Value(visitBudgetContextKey{}).(*visitBudget); ok {
		if !vb.spend(c) {
			return
		}
	}
	match([]interface{}{}, v)
	visitAll(v, func(wildcard string, v interface{}) {
		mapper(c, r, v, func(key interface{}, v interface{}) {
			match(append([]interface{}{wildcard}, key.([]interface{})...), v)
		})
	})
}

func visitAll(v interface{}, visit func(key string, v interface{})) {

	switch v := v.(type) {

	case []interface{}:
		for i, e := range v {
			k := strconv.Itoa(i)
			visit(k, e)
		}

	case map[string]interface{}:
		// Iterate object members in lexicographic key order so wildcard,
		// filter and recursive-descent results are reproducible.
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			visit(k, v[k])
		}

	case Array:
		v.ForEach(visit)

	case Object:
		v.ForEach(visit)
	}
}

// [? ]
type filterSelector struct {
	filter gval.Evaluable
}

func (f filterSelector) selectAmbiguous(c context.Context, r, v interface{}, match ambiguousMatcher) {
	visitAll(v, func(wildcard string, v interface{}) {
		condition, err := f.filter.EvalBool(currentContext(c, v), r)
		if err != nil {
			return
		}
		if condition {
			match(wildcard, v)
		}
	})
}

// [::]
type rangeSelector struct {
	min  gval.Evaluable
	max  gval.Evaluable
	step gval.Evaluable
}

func (rs rangeSelector) selectAmbiguous(c context.Context, r, v interface{}, match ambiguousMatcher) {

	c = currentContext(c, v)

	min, err := rs.min.EvalInt(c, r)
	if err != nil {
		return
	}
	max, err := rs.max.EvalInt(c, r)
	if err != nil {
		return
	}
	step, err := rs.step.EvalInt(c, r)
	if err != nil {
		return
	}

	if step == 0 {
		step = 1
	}

	// process v
	switch o := v.(type) {

	case []interface{}:
		n := len(o)
		min = negmax(min, n)
		max = negmax(max, n)

		if min > max {
			return
		}

		if step > 0 {
			for i := min; i < max; i += step {
				match(strconv.Itoa(i), o[i])
			}
		} else {
			for i := max - 1; i >= min; i += step {
				match(strconv.Itoa(i), o[i])
			}
		}

	case Array:
		n := o.Len()
		min = negmax(min, n)
		max = negmax(max, n)

		if min > max {
			return
		}

		if step > 0 {
			for i := min; i < max; i += step {
				k := strconv.Itoa(i)
				r, _ := o.SelectGVal(c, k)
				match(k, r)
			}
		} else {
			for i := max - 1; i >= min; i += step {
				k := strconv.Itoa(i)
				r, _ := o.SelectGVal(c, k)
				match(k, r)
			}
		}
	}
}

func negmax(n, max int) int {
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

// ()
type scriptSelector struct {
	script gval.Evaluable
}

func (s scriptSelector) selectPlain(c context.Context, r, v interface{}) (interface{}, interface{}, error) {
	value, err := s.script(currentContext(c, v), r)
	return nil, value, err
}

// VisitBudgetContextKey is the context key used to bound the number of nodes
// visited by recursive descent ("..") during a single evaluation. The context
// value must be an int: the maximum number of node visits. When recursive
// descent would enter more nodes than the budget the evaluation aborts with
// ErrVisitBudgetExceeded. The budget is per evaluation call, so reusing an
// evaluable never leaks a consumed budget between calls.
type VisitBudgetContextKey struct{}

// ErrVisitBudgetExceeded is returned when recursive descent exhausts the
// visit budget configured via VisitBudgetContextKey.
var ErrVisitBudgetExceeded = errors.New("jsonpath: recursive descent visit budget exceeded")

type visitBudgetContextKey struct{}

type visitBudget struct {
	limit   int // negative means unlimited
	visited int
	err     error
}

func (vb *visitBudget) spend(c context.Context) bool {
	if vb.err != nil {
		return false
	}
	if err := c.Err(); err != nil {
		vb.err = err
		return false
	}
	if vb.limit >= 0 && vb.visited >= vb.limit {
		vb.err = fmt.Errorf("%w (limit %d)", ErrVisitBudgetExceeded, vb.limit)
		return false
	}
	vb.visited++
	return true
}

// withVisitControl installs a per-evaluation visit budget and cancellation
// guard. It runs on the production evaluation path for every parsed JSONPath
// and placeholder object; absent configuration it is observable only through
// cancellation / budget errors and never alters a successful result.
func withVisitControl(eval gval.Evaluable) gval.Evaluable {
	return func(ctx context.Context, v interface{}) (interface{}, error) {
		if _, ok := ctx.Value(visitBudgetContextKey{}).(*visitBudget); ok {
			// An enclosing evaluation already owns the budget (e.g. a
			// placeholder object containing a recursive descent), so this
			// nested evaluation shares it instead of resetting it.
			return eval(ctx, v)
		}
		limit := -1
		if l, ok := ctx.Value(VisitBudgetContextKey{}).(int); ok {
			limit = l
		}
		vb := &visitBudget{limit: limit}
		ctx = context.WithValue(ctx, visitBudgetContextKey{}, vb)
		result, err := eval(ctx, v)
		if err != nil {
			return nil, err
		}
		if vb.err != nil {
			return nil, vb.err
		}
		return result, nil
	}
}
