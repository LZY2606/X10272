package jsonpath

import (
	"context"
	"errors"
)

// ErrVisitBudgetExhausted is the causal error returned when recursive
// descent (..) visits more nodes than the VisitBudget carried by the
// evaluation context allows.
var ErrVisitBudgetExhausted = errors.New("jsonpath: recursive descent visit budget exhausted")

// VisitBudget bounds how many nodes recursive descent (..) may visit during
// a single evaluation. A budget is attached to the evaluation context with
// VisitBudgetContextKey and is spent by the same mapper selector that walks
// the value tree on the production path. A nil budget (no context value)
// means unlimited visits, which keeps the default behavior unchanged.
//
// A VisitBudget is single use: once exhausted it stays exhausted and Err
// reports the causal error. Create a fresh budget per evaluation.
type VisitBudget struct {
	remaining int
	err       error
}

// NewVisitBudget returns a VisitBudget allowing limit node visits.
// A negative limit is treated as zero.
func NewVisitBudget(limit int) *VisitBudget {
	if limit < 0 {
		limit = 0
	}
	return &VisitBudget{remaining: limit}
}

// spend records one visited node and reports whether the visit is allowed.
func (b *VisitBudget) spend() bool {
	if b.err != nil {
		return false
	}
	if b.remaining <= 0 {
		b.err = ErrVisitBudgetExhausted
		return false
	}
	b.remaining--
	return true
}

// Err returns the causal error recorded when the budget was exhausted,
// or nil while the budget has not been exceeded.
func (b *VisitBudget) Err() error {
	return b.err
}

// VisitBudgetContextKey is the context key used to attach a *VisitBudget
// to an evaluation context. Exported for testing purposes.
type VisitBudgetContextKey struct{}

func visitBudgetFromContext(c context.Context) *VisitBudget {
	b, _ := c.Value(VisitBudgetContextKey{}).(*VisitBudget)
	return b
}

// allowVisit reports whether a recursive descent traversal may visit
// another node. It returns false when the context is cancelled or the
// attached VisitBudget is exhausted.
func allowVisit(c context.Context) bool {
	if c.Err() != nil {
		return false
	}
	if b := visitBudgetFromContext(c); b != nil {
		return b.spend()
	}
	return true
}

// visitError returns the causal error of an aborted traversal, if any:
// an exhausted visit budget first, then context cancellation.
func visitError(c context.Context) error {
	if b := visitBudgetFromContext(c); b != nil && b.Err() != nil {
		return b.Err()
	}
	return c.Err()
}
