package jsonpath

import (
	"context"
	"errors"
	"fmt"
)

// RecursiveDescentBudgetContextKey limits the number of nodes visited while a
// single evaluation performs a recursive descent ('..'). The context value
// must be an int. It is primarily intended for tests and callers evaluating
// untrusted or cyclic documents.
type RecursiveDescentBudgetContextKey struct{}

// RecursiveDescentDepthContextKey limits the nesting depth of a single
// recursive descent ('..'). The context value must be an int. A depth limit
// bounds recursion even when a cyclic document fans out exponentially before
// the visit count is exhausted.
type RecursiveDescentDepthContextKey struct{}

// DefaultRecursiveDescentBudget is the visit budget used when the evaluation
// context does not carry an explicit RecursiveDescentBudgetContextKey.
const DefaultRecursiveDescentBudget = 10000

// DefaultRecursiveDescentDepth is the nesting depth used when the evaluation
// context does not carry an explicit RecursiveDescentDepthContextKey.
const DefaultRecursiveDescentDepth = 1000

// ErrRecursiveDescentBudget is the causal error returned when a recursive
// descent exceeds its visit budget or nesting depth. Use errors.Is to match
// it.
var ErrRecursiveDescentBudget = errors.New("recursive descent visit budget exceeded")

// RecursiveDescentBudgetError carries the configured limit together with the
// ErrRecursiveDescentBudget cause.
type RecursiveDescentBudgetError struct {
	Visits int
	Depth  int
}

func (e *RecursiveDescentBudgetError) Error() string {
	return fmt.Sprintf("%s after %d visits at depth %d", ErrRecursiveDescentBudget, e.Visits, e.Depth)
}

func (e *RecursiveDescentBudgetError) Unwrap() error {
	return ErrRecursiveDescentBudget
}

type visitStateContextKey struct{}

type visitState struct {
	descentVisits int
	descentDepth  int
	budget        int
	maxDepth      int
	err           error
}

func newVisitContext(ctx context.Context) context.Context {
	if ctx.Value(visitStateContextKey{}) != nil {
		return ctx
	}
	state := &visitState{
		budget:   DefaultRecursiveDescentBudget,
		maxDepth: DefaultRecursiveDescentDepth,
	}
	if b, ok := ctx.Value(RecursiveDescentBudgetContextKey{}).(int); ok && b >= 0 {
		state.budget = b
	}
	if d, ok := ctx.Value(RecursiveDescentDepthContextKey{}).(int); ok && d >= 0 {
		state.maxDepth = d
	}
	return context.WithValue(ctx, visitStateContextKey{}, state)
}

func visitStateFrom(ctx context.Context) *visitState {
	state, _ := ctx.Value(visitStateContextKey{}).(*visitState)
	return state
}

// chargeDescentVisit accounts one node visited by a recursive descent and
// records the first cancellation, depth or budget error. Recursive descent is
// the only selector that follows self references, so the limits also bound
// evaluation of cyclic documents. The returned state is shared through the
// context: once it carries an error every sibling branch stops fanning out.
func chargeDescentVisit(ctx context.Context) context.Context {
	state := visitStateFrom(ctx)
	if state == nil {
		ctx = newVisitContext(ctx)
		state = visitStateFrom(ctx)
	}
	if state.err != nil {
		return ctx
	}
	if err := ctx.Err(); err != nil {
		state.err = err
		return ctx
	}
	state.descentDepth++
	state.descentVisits++
	if state.descentDepth > state.maxDepth || state.descentVisits > state.budget {
		state.err = &RecursiveDescentBudgetError{Visits: state.descentVisits, Depth: state.descentDepth}
	}
	return ctx
}

// releaseDescentDepth leaves one recursive-descent nesting level.
func releaseDescentDepth(ctx context.Context) {
	state := visitStateFrom(ctx)
	if state != nil && state.descentDepth > 0 {
		state.descentDepth--
	}
}

func visitAborted(ctx context.Context) bool {
	state := visitStateFrom(ctx)
	return state != nil && state.err != nil
}

func visitStateError(ctx context.Context) error {
	state := visitStateFrom(ctx)
	if state == nil {
		return nil
	}
	return state.err
}
