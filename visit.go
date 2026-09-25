package jsonpath

import (
	"context"
	"fmt"
)

// VisitBudgetContextKey limits the number of nodes visited by recursive
// descent ('..') selectors during a single evaluation.
// The value must be an int <= 0 disables the limit.
//
// When the budget is exhausted the evaluation fails with an error wrapping
// ErrVisitBudgetExceeded instead of silently returning partial results.
// The state is bound to the evaluation context: a reused evaluable does not
// carry visits or errors from a previous call.
type VisitBudgetContextKey struct{}

// RecursionLimitContextKey limits the recursion depth of recursive descent
// ('..') selectors during a single evaluation.
// The value must be an int <= 0 disables the limit.
type RecursionLimitContextKey struct{}

// ErrVisitBudgetExceeded signals that a JSONPath evaluation visited more
// nodes than allowed by VisitBudgetContextKey.
var ErrVisitBudgetExceeded = fmt.Errorf("jsonpath: recursive descent visit budget exceeded")

// ErrRecursionLimitExceeded signals that a JSONPath evaluation descended
// deeper than allowed by RecursionLimitContextKey.
var ErrRecursionLimitExceeded = fmt.Errorf("jsonpath: recursive descent depth limit exceeded")

const defaultVisitBudget = 10000
const defaultRecursionLimit = 1000

type visitState struct {
	visits  int
	budget  int
	maxDepth int
	abort   error
}

func visitStateFromContext(c context.Context) *visitState {
	if vs, ok := c.Value(visitStateContextKey{}).(*visitState); ok {
		return vs
	}
	vs := &visitState{budget: defaultVisitBudget, maxDepth: defaultRecursionLimit}
	if b, ok := c.Value(VisitBudgetContextKey{}).(int); ok {
		vs.budget = b
	}
	if d, ok := c.Value(RecursionLimitContextKey{}).(int); ok {
		vs.maxDepth = d
	}
	return vs
}

type visitStateContextKey struct{}

func withVisitState(c context.Context) context.Context {
	if _, ok := c.Value(visitStateContextKey{}).(*visitState); ok {
		return c
	}
	return context.WithValue(c, visitStateContextKey{}, visitStateFromContext(c))
}

// enterNode accounts one recursive-descent visit at the given depth and
// returns the causal error (cancellation, budget or depth limit) if the
// evaluation must abort. It is part of the production evaluation path and is
// shared by every selector that traverses children.
func enterNode(c context.Context, depth int) error {
	if err := c.Err(); err != nil {
		return err
	}
	vs := visitStateFromContext(c)
	if vs.maxDepth > 0 && depth > vs.maxDepth {
		return ErrRecursionLimitExceeded
	}
	if vs.budget > 0 {
		vs.visits++
		if vs.visits > vs.budget {
			return ErrVisitBudgetExceeded
		}
	}
	return nil
}

func (vs *visitState) markAborted(err error) {
	if vs.abort == nil && err != nil {
		vs.abort = err
	}
}
