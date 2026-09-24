package jsonpath

import (
	"context"
	"fmt"
	"sort"
	"strconv"

	"github.com/PaesslerAG/gval"
)

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

// defaultRecursiveVisitBudget bounds the number of nodes a single recursive
// descent (..) evaluation may visit, so cyclic or extremely deep structures
// cannot make an evaluation run forever.
const defaultRecursiveVisitBudget = 100000

// RecursiveVisitBudgetContextKey can be set to an int on the evaluation
// context to override defaultRecursiveVisitBudget for a single evaluation.
//
// Exported for testing purposes
type RecursiveVisitBudgetContextKey struct{}

// recursiveVisitBudget is the per-evaluation state for recursive descent.
// It is created fresh for every top level evaluation and carried through the
// context, so a failed or exhausted evaluation can never leak state into a
// reused Evaluable.
type recursiveVisitBudget struct {
	remaining int
	err       error
}

type recursiveVisitBudgetContextKey struct{}

// withRecursiveVisitBudget returns a context carrying the recursive descent
// visit budget and the budget itself. If a budget is already present it is
// reused, so nested ambiguous paths share one budget per evaluation.
func withRecursiveVisitBudget(c context.Context) (context.Context, *recursiveVisitBudget) {
	if b, ok := c.Value(recursiveVisitBudgetContextKey{}).(*recursiveVisitBudget); ok {
		return c, b
	}
	max := defaultRecursiveVisitBudget
	if m, ok := c.Value(RecursiveVisitBudgetContextKey{}).(int); ok {
		max = m
	}
	b := &recursiveVisitBudget{remaining: max}
	return context.WithValue(c, recursiveVisitBudgetContextKey{}, b), b
}

// visit records one visited node. It returns false once the evaluation must
// stop, either because the context was cancelled or because the visit budget
// is exhausted; the causal error is kept on the budget.
func (b *recursiveVisitBudget) visit(c context.Context) bool {
	if b.err != nil {
		return false
	}
	if err := c.Err(); err != nil {
		b.err = err
		return false
	}
	if b.remaining <= 0 {
		b.err = fmt.Errorf("jsonpath: recursive descent visit budget exhausted")
		return false
	}
	b.remaining--
	return true
}

func mapper(c context.Context, r, v interface{}, match ambiguousMatcher) {
	if b, ok := c.Value(recursiveVisitBudgetContextKey{}).(*recursiveVisitBudget); ok {
		if !b.visit(c) {
			return
		}
	} else if c.Err() != nil {
		return
	}
	match([]interface{}{}, v)
	visitAll(v, func(wildcard string, v interface{}) {
		mapper(c, r, v, func(key interface{}, v interface{}) {
			match(append([]interface{}{wildcard}, key.([]interface{})...), v)
		})
	})
}

// visitAll enumerates the direct children of v. Object keys are visited in
// sorted order so that wildcard and recursive descent evaluations produce a
// reproducible match order independent of Go map iteration.
func visitAll(v interface{}, visit func(key string, v interface{})) {

	switch v := v.(type) {

	case []interface{}:
		for i, e := range v {
			k := strconv.Itoa(i)
			visit(k, e)
		}

	case map[string]interface{}:
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
