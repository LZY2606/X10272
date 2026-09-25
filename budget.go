package jsonpath

import (
	"context"
	"fmt"
)

// RecursiveDescentBudgetContextKey limits the number of JSON values a single
// recursive descent ('..') selector is allowed to visit during one evaluation.
// The value must be an int. A value <= 0 disables the limit.
//
// The budget applies per evaluation: every evaluable call gets a fresh counter,
// so exhausting the budget (or cancelling the context) can never leak state
// into a later call that reuses the same evaluable.
type RecursiveDescentBudgetContextKey struct{}

// ErrRecursiveDescentBudgetExhausted is returned when a recursive descent
// selector visits more values than the configured
// RecursiveDescentBudgetContextKey allows.
var ErrRecursiveDescentBudgetExhausted = fmt.Errorf("jsonpath: recursive descent visit budget exhausted")

// defaultRecursiveDescentBudget bounds recursive descent on cyclic or
// pathologically deep inputs when no explicit budget is configured.
const defaultRecursiveDescentBudget = 100000

type recursiveDescentBudget struct {
	remaining int
	unlimited bool
}

func installRecursiveDescentBudget(ctx context.Context) context.Context {
	if ctx.Value(recursiveDescentBudgetKey{}) != nil {
		return ctx
	}
	limit := defaultRecursiveDescentBudget
	if v, ok := ctx.Value(RecursiveDescentBudgetContextKey{}).(int); ok {
		limit = v
	}
	return context.WithValue(ctx, recursiveDescentBudgetKey{}, &recursiveDescentBudget{
		remaining: limit,
		unlimited: limit <= 0,
	})
}

type recursiveDescentBudgetKey struct{}

// chargeRecursiveDescentVisit accounts one visited value for every recursive
// descent selector. It returns the context error (cancellation/deadline) when
// the context is done, or ErrRecursiveDescentBudgetExhausted when the budget is
// spent.
func chargeRecursiveDescentVisit(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b, ok := ctx.Value(recursiveDescentBudgetKey{}).(*recursiveDescentBudget)
	if !ok || b.unlimited {
		return nil
	}
	if b.remaining <= 0 {
		return ErrRecursiveDescentBudgetExhausted
	}
	b.remaining--
	return nil
}
