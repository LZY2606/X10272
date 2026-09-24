package jsonpath

import (
	"context"
	"fmt"
	"strconv"

	"github.com/PaesslerAG/gval"
)

// plainSelector evaluate exactly one result
type plainSelector func(c context.Context, r, v interface{}) (interface{}, interface{}, error)

// ambiguousSelector evaluate wildcard. A traversal error (cancellation or an
// exhausted visit budget) aborts the whole evaluation; per-match errors are
// swallowed by the matching logic.
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
		return visitAll(c, v, func(key string, val interface{}) { match(key, val) })
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
			match(wildcard, e)
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
	match([]interface{}{}, v)
	return visitAll(c, v, func(wildcard string, child interface{}) {
		mapper(c, r, child, func(key interface{}, cv interface{}) {
			match(append([]interface{}{wildcard}, key.([]interface{})...), cv)
		})
	})
}

// [? ]
func filterSelector(filter gval.Evaluable) ambiguousSelector {
	return func(c context.Context, r, v interface{}, match ambiguousMatcher) error {
		return visitAll(c, v, func(wildcard string, v interface{}) {
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

	return func(c context.Context, r, v interface{}, match ambiguousMatcher) error {

		c = currentContext(c, v)

		minI, err := min.EvalInt(c, r)
		if err != nil {
			return nil
		}
		maxI, err := max.EvalInt(c, r)
		if err != nil {
			return nil
		}
		stepI, err := step.EvalInt(c, r)
		if err != nil {
			return nil
		}

		if stepI == 0 {
			stepI = 1
		}

		// process v
		switch o := v.(type) {

		case []interface{}:
			n := len(o)
			minI = negmax(minI, n)
			maxI = negmax(maxI, n)

			if minI > maxI {
				return nil
			}

			if stepI > 0 {
				for i := minI; i < maxI; i += stepI {
					if err := guardFrom(c).enter(c); err != nil {
						return err
					}
					match(strconv.Itoa(i), o[i])
				}
			} else {
				for i := maxI - 1; i >= minI; i += stepI {
					if err := guardFrom(c).enter(c); err != nil {
						return err
					}
					match(strconv.Itoa(i), o[i])
				}
			}

		case Array:
			n := o.Len()
			minI = negmax(minI, n)
			maxI = negmax(maxI, n)

			if minI > maxI {
				return nil
			}

			if stepI > 0 {
				for i := minI; i < maxI; i += stepI {
					if err := guardFrom(c).enter(c); err != nil {
						return err
					}
					k := strconv.Itoa(i)
					rv, _ := o.SelectGVal(c, k)
					match(k, rv)
				}
			} else {
				for i := maxI - 1; i >= minI; i += stepI {
					if err := guardFrom(c).enter(c); err != nil {
						return err
					}
					k := strconv.Itoa(i)
					rv, _ := o.SelectGVal(c, k)
					match(k, rv)
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
