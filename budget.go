package jsonpath

import (
	"context"
	"errors"
)

// ErrVisitBudgetExhausted is returned by evaluation when recursive descent
// stops because the visit budget carried by the context is exhausted.
var ErrVisitBudgetExhausted = errors.New("jsonpath: recursive descent visit budget exhausted")

// VisitBudgetContextKey is a context key holding a *int that budgets how many
// nodes recursive descent (..) may still visit. Evaluation decrements the
// counter on every visited node; once the budget is spent recursion stops and
// the evaluation returns ErrVisitBudgetExhausted. A missing or nil budget
// means unbounded recursion.
//
// Exported for testing purposes
type VisitBudgetContextKey struct{}

// spendVisitBudget is part of the production recursive descent path (mapper);
// it is not a test-only copy of the traversal logic.
func spendVisitBudget(c context.Context) bool {
	budget, ok := c.Value(VisitBudgetContextKey{}).(*int)
	if !ok || budget == nil {
		return true
	}
	if *budget <= 0 {
		*budget = -1
		return false
	}
	*budget--
	return true
}

func visitBudgetExhausted(c context.Context) bool {
	budget, ok := c.Value(VisitBudgetContextKey{}).(*int)
	return ok && budget != nil && *budget < 0
}
