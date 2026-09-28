package jsonpath

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
)

// TraversalBudgetExceeded is the causal error returned when a recursive
// descent exhausts the visit budget configured via WithVisitBudget.
var TraversalBudgetExceeded = errors.New("jsonpath: recursive descent visit budget exceeded")

type visitBudgetContextKey struct{}

// WithVisitBudget bounds the number of nodes a recursive descent (..) is
// allowed to visit during a single evaluation. The budget is attached to the
// returned context and is consumed per visited node, including the node the
// descent starts on. A non-positive budget aborts on the first visited node.
// Without a budget recursive descent keeps the historical unlimited behavior.
func WithVisitBudget(c context.Context, visits int64) context.Context {
	return context.WithValue(c, visitBudgetContextKey{}, visits)
}

// traversalAbort is an internal control flow signal. It must never escape a
// public Evaluable: every entry point that can trigger a recursive descent
// recovers it and converts it into a causal error.
type traversalAbort struct{ err error }

// enterTraversal marks the visit of one node by a recursive descent. It
// enforces cancellation and the visit budget through a shared, context based
// counter, so sibling branches can never silently reset the budget.
func enterTraversal(c context.Context) context.Context {
	if c.Err() != nil {
		panic(traversalAbort{err: c.Err()})
	}
	if b, ok := c.Value(visitBudgetContextKey{}).(int64); ok {
		if b <= 0 {
			panic(traversalAbort{err: fmt.Errorf("%w: %d visits remaining", TraversalBudgetExceeded, b)})
		}
		c = context.WithValue(c, visitBudgetContextKey{}, b-1)
	}
	return c
}

func recoverTraversal(errp *error) {
	switch r := recover().(type) {
	case nil:
	case traversalAbort:
		*errp = r.err
	default:
		panic(r)
	}
}

// forEachObject visits object members in a reproducible (lexicographically
// sorted by key) order. Only JSON decoded map[string]interface{} values are
// reordered; custom Object implementations keep their own ForEach order.
func forEachObject(m map[string]interface{}, visit func(key string, v interface{})) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		visit(k, m[k])
	}
}

func itoa(i int) string { return strconv.Itoa(i) }
