package jsonpath

import (
	"context"
	"errors"

	"github.com/PaesslerAG/gval"
)

// ErrVisitBudgetExhausted is the causal error recorded when recursive
// descent (..) exhausts the VisitBudget carried by the evaluation context.
var ErrVisitBudgetExhausted = errors.New("jsonpath: recursive descent visit budget exhausted")

// VisitBudgetContextKey is exported for testing purposes, mirroring
// CollectFullPathsContextKey. A *VisitBudget stored under this key limits
// how many nodes recursive descent (..) may visit during one evaluation.
// Without a budget in the context recursive descent is unlimited, which
// preserves the historic default behavior.
type VisitBudgetContextKey struct{}

// VisitBudget is a per-evaluation counter shared with the production
// evaluation path: recursive descent spends one visit per reached node.
// A budget belongs to a single evaluation and must not be shared across
// concurrent evaluations.
type VisitBudget struct {
	remaining int
	err       error
}

// NewVisitBudget returns a VisitBudget allowing exactly visits node visits.
func NewVisitBudget(visits int) *VisitBudget {
	if visits < 0 {
		visits = 0
	}
	return &VisitBudget{remaining: visits}
}

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
// or nil while the budget is not exhausted.
func (b *VisitBudget) Err() error {
	return b.err
}

func spendVisitBudget(c context.Context) bool {
	b, ok := c.Value(VisitBudgetContextKey{}).(*VisitBudget)
	if !ok || b == nil {
		return true
	}
	return b.spend()
}

func visitBudgetErr(c context.Context) error {
	b, ok := c.Value(VisitBudgetContextKey{}).(*VisitBudget)
	if !ok || b == nil {
		return nil
	}
	return b.Err()
}

// guardEvaluable wraps the production path evaluation with context
// cancellation and visit-budget checks. It shares the exact evaluation
// logic of the wrapped function and only surfaces causal errors that the
// evaluation itself cannot return (recursive descent reports matches
// through error-less callbacks).
func guardEvaluable(eval func(context.Context, interface{}) (interface{}, error)) gval.Evaluable {
	return func(ctx context.Context, parameter interface{}) (interface{}, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		v, err := eval(ctx, parameter)
		if err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := visitBudgetErr(ctx); err != nil {
			return nil, err
		}
		return v, nil
	}
}
