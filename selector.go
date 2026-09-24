package jsonpath

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"

	"github.com/PaesslerAG/gval"
)

// RecursiveDescentBudgetContextKey limits how many values a single evaluation
// visits while executing a recursive descent selector (".."). The context
// value must be a positive int. It is exported so callers (and tests) can bound
// recursive descent through the public evaluation entry points without any
// side channel.
type RecursiveDescentBudgetContextKey struct{}

// ErrRecursiveDescentBudgetExceeded is the causal error returned when a
// recursive descent visits more values than the configured
// RecursiveDescentBudgetContextKey budget.
var ErrRecursiveDescentBudgetExceeded = errors.New("jsonpath: recursive descent visit budget exceeded")

// traversalState is created fresh for every evaluation and never shared
// between evaluable calls. It carries the per-evaluation recursive descent
// counter as well as the first causal failure (budget exhaustion or context
// cancellation).
type traversalState struct {
	visits int
	err    error
}

type traversalStateContextKey struct{}

// beginTraversal installs a fresh traversalState when the caller bound a
// recursive descent budget or a cancellable context. Evaluations without
// either keep the original, allocation free behaviour.
func beginTraversal(c context.Context) (context.Context, *traversalState) {
	_, hasBudget := c.Value(RecursiveDescentBudgetContextKey{}).(int)
	if !hasBudget && c.Done() == nil {
		return c, nil
	}
	state := &traversalState{}
	return context.WithValue(c, traversalStateContextKey{}, state), state
}

func traversalStateOf(c context.Context) *traversalState {
	state, _ := c.Value(traversalStateContextKey{}).(*traversalState)
	return state
}

// traversalFailed reports the first causal failure of the current evaluation,
// if any. Ambiguous selectors stop emitting matches once it is set so a failed
// traversal can never contribute partial results.
func traversalFailed(c context.Context) error {
	if state := traversalStateOf(c); state != nil {
		return state.err
	}
	return nil
}

// plainSelector evaluate exactly one result
type plainSelector func(c context.Context, r, v interface{}) (interface{}, interface{}, error)

// ambiguousSelector evaluate wildcard
type ambiguousSelector func(c context.Context, r, v interface{}, match ambiguousMatcher)

// @
func currentElementSelector() plainSelector {
	return func(c context.Context, r, v interface{}) (interface{}, interface{}, error) {
		return nil, c.Value(currentElement{}), nil
	}
}

type currentElement struct{}

func currentContext(c context.Context, v interface{}) context.Context {
	return context.WithValue(c, currentElement{}, v)
}

// .x, [x]
func directSelector(key gval.Evaluable) plainSelector {
	return func(c context.Context, r, v interface{}) (interface{}, interface{}, error) {

		e, k, err := selectValue(c, key, r, v)
		if err != nil {
			return nil, nil, err
		}

		return k, e, nil
	}
}

// * / [*]
func starSelector() ambiguousSelector {
	return func(c context.Context, r, v interface{}, match ambiguousMatcher) {
		visitAll(v, func(key string, val interface{}) { match(key, val) })
	}
}

// [x, ...]
func multiSelector(keys []gval.Evaluable) ambiguousSelector {
	if len(keys) == 0 {
		return starSelector()
	}
	return func(c context.Context, r, v interface{}, match ambiguousMatcher) {
		for _, k := range keys {
			e, wildcard, err := selectValue(c, k, r, v)
			if err != nil {
				continue
			}
			match(wildcard, e)
		}
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
func mapperSelector() ambiguousSelector {
	return mapper
}

func mapper(c context.Context, r, v interface{}, match ambiguousMatcher) {
	if !chargeRecursiveDescent(c) {
		return
	}
	match([]interface{}{}, v)
	visitAll(v, func(wildcard string, v interface{}) {
		mapper(c, r, v, func(key interface{}, v interface{}) {
			match(append([]interface{}{wildcard}, key.([]interface{})...), v)
		})
	})
}

// chargeRecursiveDescent accounts for one visited value of a recursive
// descent. It enforces the per-evaluation visit budget and honours context
// cancellation, recording the first causal failure on the traversalState.
func chargeRecursiveDescent(c context.Context) bool {
	state := traversalStateOf(c)
	if state == nil {
		return true
	}
	if state.err != nil {
		return false
	}
	if err := c.Err(); err != nil {
		state.err = err
		return false
	}
	if budget, ok := c.Value(RecursiveDescentBudgetContextKey{}).(int); ok {
		state.visits++
		if state.visits > budget {
			state.err = ErrRecursiveDescentBudgetExceeded
			return false
		}
	}
	return true
}

func visitAll(v interface{}, visit func(key string, v interface{})) {

	switch v := v.(type) {

	case []interface{}:
		for i, e := range v {
			k := strconv.Itoa(i)
			visit(k, e)
		}

	case map[string]interface{}:
		visitSortedMap(keysOf(v), v, visit)

	case Array:
		v.ForEach(visit)

	case Object:
		v.ForEach(visit)
	}
}

func keysOf(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func visitSortedMap(keys []string, m map[string]interface{}, visit func(key string, v interface{})) {
	sort.Strings(keys)
	for _, k := range keys {
		visit(k, m[k])
	}
}

// [? ]
func filterSelector(filter gval.Evaluable) ambiguousSelector {
	return func(c context.Context, r, v interface{}, match ambiguousMatcher) {
		visitAll(v, func(wildcard string, v interface{}) {
			condition, err := filter.EvalBool(currentContext(c, v), r)
			if err != nil {
				return
			}
			if condition {
				match(wildcard, v)
			}
		})
	}
}

// [::]
func rangeSelector(min, max, step gval.Evaluable) ambiguousSelector {

	return func(c context.Context, r, v interface{}, match ambiguousMatcher) {

		c = currentContext(c, v)

		min, err := min.EvalInt(c, r)
		if err != nil {
			return
		}
		max, err := max.EvalInt(c, r)
		if err != nil {
			return
		}
		step, err := step.EvalInt(c, r)
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
func newScript(script gval.Evaluable) plainSelector {
	return func(c context.Context, r, v interface{}) (interface{}, interface{}, error) {
		value, err := script(currentContext(c, v), r)
		return nil, value, err
	}
}
