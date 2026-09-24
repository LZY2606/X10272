package jsonpath

import (
	"context"
	"errors"
	"sort"
	"strconv"
)

// VisitBudgetContextKey is exported for testing purposes.
// When a context carrying this key (with a positive *int value) is used to
// parse and evaluate a JSONPath, the value limits the number of values
// visited while traversing the document (wildcards, recursive descent).
// The counter is attached to the context for a single evaluation and is
// therefore never shared between evaluations of the same evaluable.
type VisitBudgetContextKey struct{}

// DeterministicVisitOrderContextKey is exported for testing purposes.
// When a context carrying this key with the bool value true is used to
// parse and evaluate a JSONPath, object members are enumerated in sorted key
// order. Array order is always deterministic.
type DeterministicVisitOrderContextKey struct{}

// ErrVisitBudgetExceeded is returned when a document traversal exhausts the
// configured VisitBudgetContextKey budget. It wraps context.Canceled style
// causation: callers can check it with errors.Is.
var ErrVisitBudgetExceeded = errors.New("jsonpath: visit budget exceeded")

type visitGuardContextKey struct{}

type visitGuard struct {
	budget int
	used   int
}

// visitGuardFor returns the per-evaluation visit guard. The guard is created
// exactly once per evaluation inside the root evaluable and stored in a
// child context, so counters cannot leak into, or be reused from, another
// evaluation.
func visitGuardFor(ctx context.Context) context.Context {
	if ctx.Value(visitGuardContextKey{}) != nil {
		return ctx
	}
	budget := 0
	if b, ok := ctx.Value(VisitBudgetContextKey{}).(*int); ok && b != nil {
		budget = *b
	}
	return context.WithValue(ctx, visitGuardContextKey{}, &visitGuard{budget: budget})
}

func guardFrom(ctx context.Context) *visitGuard {
	g, _ := ctx.Value(visitGuardContextKey{}).(*visitGuard)
	if g != nil {
		return g
	}
	return &visitGuard{}
}

// enter is called once for every value reached while traversing the
// document. The root itself is not counted. Cancellation is always honored;
// the budget is opt-in.
func (g *visitGuard) enter(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if g.budget > 0 {
		g.used++
		if g.used > g.budget {
			return ErrVisitBudgetExceeded
		}
	}
	return nil
}

func deterministicOrder(ctx context.Context) bool {
	b, _ := ctx.Value(DeterministicVisitOrderContextKey{}).(bool)
	return b
}

func visitAll(ctx context.Context, v interface{}, visit func(key string, v interface{})) error {

	switch v := v.(type) {

	case []interface{}:
		for i, e := range v {
			if err := guardFrom(ctx).enter(ctx); err != nil {
				return err
			}
			visit(strconv.Itoa(i), e)
		}

	case map[string]interface{}:
		if deterministicOrder(ctx) {
			keys := make([]string, 0, len(v))
			for k := range v {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				if err := guardFrom(ctx).enter(ctx); err != nil {
					return err
				}
				visit(k, v[k])
			}
			return nil
		}
		for k, e := range v {
			if err := guardFrom(ctx).enter(ctx); err != nil {
				return err
			}
			visit(k, e)
		}

	case Array:
		var err error
		v.ForEach(func(key string, val interface{}) {
			if err != nil {
				return
			}
			if e := guardFrom(ctx).enter(ctx); e != nil {
				err = e
				return
			}
			visit(key, val)
		})
		return err

	case Object:
		var err error
		v.ForEach(func(key string, val interface{}) {
			if err != nil {
				return
			}
			if e := guardFrom(ctx).enter(ctx); e != nil {
				err = e
				return
			}
			visit(key, val)
		})
		return err
	}
	return nil
}
