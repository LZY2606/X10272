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
		var abort error
		visitAll(v, func(key string, val interface{}) {
			if abort != nil {
				return
			}
			abort = match(key, val)
		})
		return abort
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
	if err := chargeRecursiveDescentVisit(c); err != nil {
		return err
	}
	if err := match([]interface{}{}, v); err != nil {
		return err
	}
	var abort error
	visitAll(v, func(wildcard string, child interface{}) {
		if abort != nil {
			return
		}
		abort = mapper(c, r, child, func(key interface{}, cv interface{}) error {
			return match(append([]interface{}{wildcard}, key.([]interface{})...), cv)
		})
	})
	return abort
}

// visitAll visits arrays in index order and objects in sorted key order, so
// that wildcard and recursive descent results are reproducible across runs.
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
	return func(c context.Context, r, v interface{}, match ambiguousMatcher) error {
		var abort error
		visitAll(v, func(wildcard string, v interface{}) {
			if abort != nil {
				return
			}
			condition, err := filter.EvalBool(currentContext(c, v), r)
			if err != nil {
				return
			}
			if condition {
				abort = match(wildcard, v)
			}
		})
		return abort
	}
}

// [::]
func rangeSelector(min, max, step gval.Evaluable) ambiguousSelector {

	return func(c context.Context, r, v interface{}, match ambiguousMatcher) error {

		c = currentContext(c, v)

		lo, err := min.EvalInt(c, r)
		if err != nil {
			return nil
		}
		hi, err := max.EvalInt(c, r)
		if err != nil {
			return nil
		}
		st, err := step.EvalInt(c, r)
		if err != nil {
			return nil
		}

		if st == 0 {
			st = 1
		}

		var abort error
		emit := func(k string, val interface{}) {
			if abort == nil {
				abort = match(k, val)
			}
		}

		// process v
		switch o := v.(type) {

		case []interface{}:
			n := len(o)
			lo = negmax(lo, n)
			hi = negmax(hi, n)

			if lo > hi {
				return nil
			}

			if st > 0 {
				for i := lo; i < hi; i += st {
					emit(strconv.Itoa(i), o[i])
					if abort != nil {
						return abort
					}
				}
			} else {
				for i := hi - 1; i >= lo; i += st {
					emit(strconv.Itoa(i), o[i])
					if abort != nil {
						return abort
					}
				}
			}

		case Array:
			n := o.Len()
			lo = negmax(lo, n)
			hi = negmax(hi, n)

			if lo > hi {
				return nil
			}

			if st > 0 {
				for i := lo; i < hi; i += st {
					k := strconv.Itoa(i)
					val, _ := o.SelectGVal(c, k)
					emit(k, val)
					if abort != nil {
						return abort
					}
				}
			} else {
				for i := hi - 1; i >= lo; i += st {
					k := strconv.Itoa(i)
					val, _ := o.SelectGVal(c, k)
					emit(k, val)
					if abort != nil {
						return abort
					}
				}
			}
		}
		return abort
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
