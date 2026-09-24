package jsonpath

import (
	"context"
	"fmt"
)

// DescentBudgetContextKey carries the maximum number of nodes a single
// JSONPath evaluation may visit through recursive descent ('..').
// The context value must be an int >= 0. The root node of the descent
// counts as one visit.
type DescentBudgetContextKey struct{}

// DescentBudgetExceededError reports that a recursive descent visited more
// nodes than the budget configured with DescentBudgetContextKey allows.
// The error is repeatable: the same evaluation on the same input with the
// same budget always fails the same way and leaves no counters behind.
type DescentBudgetExceededError struct {
	Limit int
}

func (e *DescentBudgetExceededError) Error() string {
	return fmt.Sprintf("jsonpath: recursive descent exceeded visit budget of %d", e.Limit)
}

type descentBudgetKey struct{}

type descentGuard struct {
	remaining int
	limit     int
	fatal     error
}

func withDescentGuard(ctx context.Context, budget interface{}) context.Context {
	if _, ok := ctx.Value(descentBudgetKey{}).(*descentGuard); ok {
		return ctx
	}
	limit := int(^uint(0) >> 1)
	if n, ok := budget.(int); ok {
		limit = n
	}
	return context.WithValue(ctx, descentBudgetKey{}, &descentGuard{remaining: limit, limit: limit})
}

// enter records one recursive-descent visit. Budget exhaustion and context
// cancellation are causal, call-wide failures: they are remembered on the
// guard so the top-level evaluable can surface them, while ordinary
// per-selector failures stay isolated from sibling selectors.
func (g *descentGuard) enter(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		if g.fatal == nil {
			g.fatal = err
		}
		return err
	}
	if g.remaining <= 0 {
		if g.fatal == nil {
			g.fatal = &DescentBudgetExceededError{Limit: g.limit}
		}
		return g.fatal
	}
	g.remaining--
	return nil
}
