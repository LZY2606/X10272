package jsonpath

import (
	"context"
	"fmt"
)

// MaxVisitsContextKey can be set to an int in the context of an evaluation
// to override the default recursive descent visit budget.
// Exported for testing purposes
type MaxVisitsContextKey struct{}

// DefaultMaxVisits bounds how many nodes a single evaluation may visit
// through recursive descent (..) before the evaluation is aborted.
const DefaultMaxVisits = 100000

type visitStateContextKey struct{}

// visitState is the mutable per-evaluation budget state. It is created
// fresh by every top level evaluation entry point and only carried through
// the context of that evaluation, so reused evaluators never share
// counters, caches or errors across evaluations.
type visitState struct {
	max  int
	used int
	err  error
}

func withVisitState(c context.Context) (context.Context, *visitState) {
	if s, ok := c.Value(visitStateContextKey{}).(*visitState); ok {
		return c, s
	}
	max := DefaultMaxVisits
	if m, ok := c.Value(MaxVisitsContextKey{}).(int); ok {
		max = m
	}
	s := &visitState{max: max}
	return context.WithValue(c, visitStateContextKey{}, s), s
}

// visit accounts one node visited by recursive descent. It returns false
// and records the causal error when the evaluation is cancelled or the
// visit budget is exhausted.
func (s *visitState) visit(c context.Context) bool {
	if s.err != nil {
		return false
	}
	if err := c.Err(); err != nil {
		s.err = err
		return false
	}
	s.used++
	if s.used > s.max {
		s.err = fmt.Errorf("jsonpath: recursive descent visit budget %d exceeded", s.max)
		return false
	}
	return true
}
