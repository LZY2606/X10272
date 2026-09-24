package jsonpath

import (
	"context"
	"errors"
)

// ErrRecursiveDescentBudgetExhausted is returned when a recursive descent
// ('..') visits more nodes than the visit budget carried by the evaluation
// context. It is kept unexported because the budget knob itself is an internal
// testability seam; production callers only observe the plain error.
var ErrRecursiveDescentBudgetExhausted = errors.New("jsonpath: recursive descent visit budget exhausted")

type descentStateKey struct{}
type descentLimitKey struct{}

// defaultRecursiveDescentLimit bounds the number of nodes a single evaluation
// of a recursive descent ('..') may visit. It only guards pathological inputs
// (e.g. cyclic structures) and never triggers on ordinary JSON documents.
const defaultRecursiveDescentLimit = 100000

// descentState is created fresh for every evaluation that contains a
// recursive descent. It never escapes the evaluation context, so a failed
// selector cannot leave counters or temporary state behind for a reused
// evaluator.
type descentState struct {
	visits int
	limit  int
	err    error
}

func ensureDescentState(c context.Context) context.Context {
	if c.Value(descentStateKey{}) != nil {
		return c
	}
	limit := defaultRecursiveDescentLimit
	if l, ok := c.Value(descentLimitKey{}).(int); ok && l > 0 {
		limit = l
	}
	return context.WithValue(c, descentStateKey{}, &descentState{limit: limit})
}

// chargeRecursiveVisit accounts one visited node of a recursive descent.
// Context cancellation wins over the budget so a cancelled call keeps its
// context.Canceled / context.DeadlineExceeded cause.
func chargeRecursiveVisit(c context.Context) (context.Context, error) {
	if err := c.Err(); err != nil {
		return c, err
	}
	s, ok := c.Value(descentStateKey{}).(*descentState)
	if !ok {
		return c, nil
	}
	s.visits++
	if s.visits > s.limit {
		if s.err == nil {
			s.err = ErrRecursiveDescentBudgetExhausted
		}
		return c, s.err
	}
	return c, nil
}

// recursiveDescentError reports the first causal error recorded during a
// recursive descent.
func recursiveDescentError(c context.Context) error {
	if s, ok := c.Value(descentStateKey{}).(*descentState); ok && s.err != nil {
		return s.err
	}
	return nil
}
