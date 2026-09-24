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
type plainSelector func(c context.Context, r, v interface{}) (interface{}, interface{}, error)

// ambiguousSelector evaluate wildcard.
// A returned error is fatal (e.g. context cancellation or an exhausted
// visit budget) and aborts the whole evaluation. Non-matching branches
// are not errors and must be skipped silently instead.
type ambiguousSelector func(c context.Context, r, v interface{}, match ambiguousMatcher) error

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
	return func(c context.Context, r, v interface{}, match ambiguousMatcher) error {
		return visitAll(v, func(key string, val interface{}) error { return match(key, val) })
	}
}

// [x, ...]
func multiSelector(keys []gval.Evaluable) ambiguousSelector {
	if len(keys) == 0 {
		return starSelector()
	}
	return func(c context.Context, r, v interface{}, match ambiguousMatcher) error {
		for _, k := range keys {
			e, wildcard, err := selectValue(c, k, r, v)
			if err != nil {
				continue
			}
			if err := match(wildcard, e); err != nil {
				return err
			}
		}
		return nil
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

func mapper(c context.Context, r, v interface{}, match ambiguousMatcher) error {
	if err := c.Err(); err != nil {
		return err
	}
	c, budget := visitBudget(c)
	if budget.remaining < 1 {
		return ErrVisitBudgetExhausted
	}
	budget.remaining--
	if err := match([]interface{}{}, v); err != nil {
		return err
	}
	return visitAll(v, func(wildcard string, v interface{}) error {
		return mapper(c, r, v, func(key interface{}, v interface{}) error {
			return match(append([]interface{}{wildcard}, key.([]interface{})...), v)
		})
	})
}

// ErrVisitBudgetExhausted is returned by an evaluation when recursive
// descent ($..) visits more nodes than the visit budget allows.
var ErrVisitBudgetExhausted = errors.New("jsonpath: recursive descent visit budget exhausted")

// VisitBudgetContextKey is exported for testing purposes. If a context
// carries an int value under this key, recursive descent ($..) visits at
// most that many nodes during one evaluation, otherwise defaultVisitBudget
// applies. The budget is spent per evaluation, so evaluables stay reusable.
type VisitBudgetContextKey struct{}

const defaultVisitBudget = 1 << 20

type visitCounterKey struct{}

type visitCounter struct {
	remaining int
}

func visitBudget(c context.Context) (context.Context, *visitCounter) {
	if counter, ok := c.Value(visitCounterKey{}).(*visitCounter); ok {
		return c, counter
	}
	budget := defaultVisitBudget
	if b, ok := c.Value(VisitBudgetContextKey{}).(int); ok {
		budget = b
	}
	counter := &visitCounter{remaining: budget}
	return context.WithValue(c, visitCounterKey{}, counter), counter
}

func visitAll(v interface{}, visit func(key string, v interface{}) error) error {
	switch v := v.(type) {

	case []interface{}:
		for i, e := range v {
			if err := visit(strconv.Itoa(i), e); err != nil {
				return err
			}
		}

	case map[string]interface{}:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		// sort keys so object traversal order is reproducible
		sort.Strings(keys)
		for _, k := range keys {
			if err := visit(k, v[k]); err != nil {
				return err
			}
		}

	case Array:
		return visitEach(v.ForEach, visit)

	case Object:
		return visitEach(v.ForEach, visit)
	}
	return nil
}

func visitEach(forEach func(func(string, interface{})), visit func(key string, v interface{}) error) error {
	var err error
	forEach(func(key string, v interface{}) {
		if err == nil {
			err = visit(key, v)
		}
	})
	return err
}

// [? ]
func filterSelector(filter gval.Evaluable) ambiguousSelector {
	return func(c context.Context, r, v interface{}, match ambiguousMatcher) error {
		return visitAll(v, func(wildcard string, v interface{}) error {
			condition, err := filter.EvalBool(currentContext(c, v), r)
			if err != nil {
				return nil
			}
			if condition {
				return match(wildcard, v)
			}
			return nil
		})
	}
}

// [::]
func rangeSelector(min, max, step gval.Evaluable) ambiguousSelector {

	return func(c context.Context, r, v interface{}, match ambiguousMatcher) error {

		c = currentContext(c, v)

		min, err := min.EvalInt(c, r)
		if err != nil {
			return nil
		}
		max, err := max.EvalInt(c, r)
		if err != nil {
			return nil
		}
		step, err := step.EvalInt(c, r)
		if err != nil {
			return nil
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
				return nil
			}

			if step > 0 {
				for i := min; i < max; i += step {
					if err := match(strconv.Itoa(i), o[i]); err != nil {
						return err
					}
				}
			} else {
				for i := max - 1; i >= min; i += step {
					if err := match(strconv.Itoa(i), o[i]); err != nil {
						return err
					}
				}
			}

		case Array:
			n := o.Len()
			min = negmax(min, n)
			max = negmax(max, n)

			if min > max {
				return nil
			}

			if step > 0 {
				for i := min; i < max; i += step {
					k := strconv.Itoa(i)
					r, _ := o.SelectGVal(c, k)
					if err := match(k, r); err != nil {
						return err
					}
				}
			} else {
				for i := max - 1; i >= min; i += step {
					k := strconv.Itoa(i)
					r, _ := o.SelectGVal(c, k)
					if err := match(k, r); err != nil {
						return err
					}
				}
			}
		}
		return nil
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
