package jsonpath

import (
	"context"
	"fmt"
)

// VisitBudget bounds how many nodes recursive descent (..) may visit during
// a single evaluation. It is shared with the production execution path: the
// recursive descent selector (mapper in selector.go) consumes the budget
// carried by the evaluation context, there is no separate test-only copy.
//
// A budget is not required; without one recursive descent is unbounded, which
// keeps the historical default behavior.
type VisitBudget struct {
	limit     int
	remaining int
	err       error
}

// NewVisitBudget returns a VisitBudget allowing limit recursive descent
// visits. A negative limit is treated as zero.
func NewVisitBudget(limit int) *VisitBudget {
	if limit < 0 {
		limit = 0
	}
	return &VisitBudget{limit: limit, remaining: limit}
}

// Limit returns the configured visit limit.
func (b *VisitBudget) Limit() int {
	return b.limit
}

// Remaining returns how many visits are still allowed.
func (b *VisitBudget) Remaining() int {
	return b.remaining
}

// Err returns the causal error recorded when the budget was exhausted.
// Evaluation itself stays recoverable: matched results collected before
// exhaustion are returned and a fresh budget can be used to retry.
func (b *VisitBudget) Err() error {
	return b.err
}

// visit consumes one visit and reports whether the descent may continue.
func (b *VisitBudget) visit() bool {
	if b.err != nil {
		return false
	}
	if b.remaining <= 0 {
		b.err = fmt.Errorf("jsonpath: recursive descent visit budget of %d exhausted", b.limit)
		return false
	}
	b.remaining--
	return true
}

// VisitBudgetContextKey optionally carries a *VisitBudget in the evaluation
// context to bound recursive descent visits.
//
// Exported for testing purposes
type VisitBudgetContextKey struct{}

func visitBudget(c context.Context) *VisitBudget {
	b, _ := c.Value(VisitBudgetContextKey{}).(*VisitBudget)
	return b
}
