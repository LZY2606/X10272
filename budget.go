package jsonpath

import (
	"context"
	"errors"
	"fmt"
)

// ErrVisitBudgetExceeded is returned when recursive descent (`..`) exceeds the
// visit budget of a single evaluation.
var ErrVisitBudgetExceeded = errors.New("jsonpath: recursive descent visit budget exceeded")

// DefaultMaxVisits is the default number of nodes recursive descent may visit
// during one evaluation.
const DefaultMaxVisits = 100000

// VisitBudgetContextKey can be used to overwrite the visit budget of a single
// evaluation with an int value on the evaluation context.
//
// Exported for testing purposes
type VisitBudgetContextKey struct{}

// visitBudget is the per-evaluation visit budget. A fresh budget is installed
// per evaluation call so a reused Evaluable never carries counters between
// evaluations.
type visitBudget struct {
	remaining int
	err       error
}

type visitBudgetContextKey struct{}

// budgetContext returns a context carrying the evaluation visit budget. If the
// context already carries a budget (nested evaluations share one top level
// evaluation) it is reused.
func budgetContext(c context.Context) (context.Context, *visitBudget) {
	if b, ok := c.Value(visitBudgetContextKey{}).(*visitBudget); ok {
		return c, b
	}
	max := DefaultMaxVisits
	if v, ok := c.Value(VisitBudgetContextKey{}).(int); ok && v >= 0 {
		max = v
	}
	b := &visitBudget{remaining: max}
	return context.WithValue(c, visitBudgetContextKey{}, b), b
}

// err returns the first terminal error recorded during the evaluation, if any.
func (b *visitBudget) Err() error {
	return b.err
}

// consume records one visited node. It reports false once the evaluation must
// stop: when the budget is exhausted or when the context was cancelled. The
// causal error is kept on the budget and returned by the evaluation entry.
func (b *visitBudget) consume(c context.Context) bool {
	if b.err != nil {
		return false
	}
	if err := c.Err(); err != nil {
		b.err = fmt.Errorf("jsonpath: evaluation stopped: %w", err)
		return false
	}
	if b.remaining <= 0 {
		b.err = ErrVisitBudgetExceeded
		return false
	}
	b.remaining--
	return true
}

// withBudget evaluates eval with a visit budget installed on the context and
// returns the first terminal budget error, if any. Successful results are
// returned unchanged.
func withBudget(eval func(context.Context, interface{}) (interface{}, error)) func(context.Context, interface{}) (interface{}, error) {
	return func(c context.Context, v interface{}) (interface{}, error) {
		c, budget := budgetContext(c)
		value, err := eval(c, v)
		if err != nil {
			return nil, err
		}
		if err := budget.Err(); err != nil {
			return nil, err
		}
		return value, nil
	}
}
