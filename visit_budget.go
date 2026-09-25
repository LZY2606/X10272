package jsonpath

import (
	"context"
	"fmt"
)

// VisitBudgetContextKey limits the number of nodes visited while evaluating a
// JSONPath. Only recursive descent ('..') consumes the budget, the root value
// counts as the first visit. The context value must be an int.
// A value of zero or less disables the limit.
//
// The same context value also honours context cancellation: evaluation stops
// with the cancellation cause once the context is done.
type VisitBudgetContextKey struct{}

// DefaultVisitBudget is used when no VisitBudgetContextKey is present.
const DefaultVisitBudget = 100000

type visitBudgetContextKey struct{}

type recursionGuard struct {
	ctx    context.Context
	limit  int
	visits int
	err    error
}

func withRecursionGuard(ctx context.Context) context.Context {
	if ctx.Value(visitBudgetContextKey{}) != nil {
		return ctx
	}
	limit := DefaultVisitBudget
	if n, ok := ctx.Value(VisitBudgetContextKey{}).(int); ok {
		limit = n
	}
	return context.WithValue(ctx, visitBudgetContextKey{}, &recursionGuard{ctx: ctx, limit: limit})
}

func recursionGuardFromContext(ctx context.Context) *recursionGuard {
	g, _ := ctx.Value(visitBudgetContextKey{}).(*recursionGuard)
	return g
}

// enter accounts for visiting one node during a recursive descent. All
// descendants of one evaluation share the same guard, so a failure of one
// selector branch is recorded on the shared guard while successful results
// stay free of any guard state.
func (g *recursionGuard) enter() error {
	if g == nil {
		return nil
	}
	if g.err != nil {
		return g.err
	}
	if err := g.ctx.Err(); err != nil {
		g.err = err
		return err
	}
	g.visits++
	if g.limit > 0 && g.visits > g.limit {
		g.err = fmt.Errorf("recursive descent exceeded visit budget of %d", g.limit)
		return g.err
	}
	return nil
}
