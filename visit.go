package jsonpath

import (
	"context"
	"fmt"
	"sort"
)

// ErrRecursiveDescentBudgetExceeded is returned when a recursive descent ('..')
// visits more nodes than the visit budget configured on the evaluation context.
// The budget bounds the work of a single evaluation and keeps failure
// causation: once the limit is reached no further nodes are visited.
var ErrRecursiveDescentBudgetExceeded = fmt.Errorf("jsonpath: recursive descent visit budget exceeded")

// visitBudgetContextKey limits the number of nodes visited by recursive
// descents during one evaluation. A missing value or a value <= 0 means
// unlimited. The value type is int.
type visitBudgetContextKey struct{}

// deterministicOrderContextKey forces object iteration to visit keys in
// sorted order. Array iteration always follows index order. The value type
// is bool.
type deterministicOrderContextKey struct{}

// visitState is created fresh for every evaluation entered through a public
// entry point (New/Get/GetWithPaths and the placeholder extension). It is
// never stored on a parsed evaluable, so reusing an evaluable can never leak
// counters between calls.
type visitState struct {
	visited int
	err     error
}

func (s *visitState) charge(ctx context.Context) error {
	if s.err != nil {
		return s.err
	}
	if err := ctx.Err(); err != nil {
		s.err = err
		return s.err
	}
	if limit, _ := ctx.Value(visitBudgetContextKey{}).(int); limit > 0 {
		s.visited++
		if s.visited > limit {
			s.err = ErrRecursiveDescentBudgetExceeded
			return s.err
		}
	}
	return nil
}

// withVisitScope starts a fresh per-evaluation budget scope around eval.
// Nested evaluations that already carry a scope (for example JSONPath values
// inside a placeholder object) reuse the scope of the surrounding evaluation.
func withVisitScope(eval func(context.Context, interface{}) (interface{}, error)) func(context.Context, interface{}) (interface{}, error) {
	return func(ctx context.Context, v interface{}) (interface{}, error) {
		if ctx.Value(visitStateKey{}) == nil {
			ctx = context.WithValue(ctx, visitStateKey{}, &visitState{})
		}
		return eval(ctx, v)
	}
}

type visitStateKey struct{}

func visitStateFromContext(ctx context.Context) *visitState {
	if s, ok := ctx.Value(visitStateKey{}).(*visitState); ok {
		return s
	}
	return &visitState{}
}

func deterministicOrder(ctx context.Context) bool {
	b, _ := ctx.Value(deterministicOrderContextKey{}).(bool)
	return b
}

// deterministicMapKeys returns the keys of m in sorted order when the
// context requests reproducible traversal; otherwise nil.
func deterministicMapKeys(ctx context.Context, keys []string) []string {
	if !deterministicOrder(ctx) {
		return nil
	}
	sorted := make([]string, len(keys))
	copy(sorted, keys)
	sort.Strings(sorted)
	return sorted
}
