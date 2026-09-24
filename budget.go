package jsonpath

import (
	"context"
	"errors"
)

// VisitBudgetContextKey is an exported context key that limits how many nodes
// recursive descent (..) may visit during a single path evaluation.
// Set it to a positive int to bound the traversal; a missing, zero or
// negative value means unlimited. The budget is counted per top-level
// evaluation, so a reused Evaluable never carries budget state between calls.
type VisitBudgetContextKey struct{}

// ErrVisitBudgetExceeded is returned (wrapped) when recursive descent exceeds
// the visit budget configured via VisitBudgetContextKey. It is a fatal error:
// evaluation stops instead of silently returning partial matches.
var ErrVisitBudgetExceeded = errors.New("jsonpath: recursive descent visit budget exceeded")

type visitBudgetContextKey struct{}

type visitBudget struct {
	remaining int
}

// withVisitBudget installs the per-evaluation budget carrier derived from
// VisitBudgetContextKey. It is a no-op when no budget is configured or when a
// carrier is already present, so nested evaluations share one budget.
func withVisitBudget(ctx context.Context) context.Context {
	if ctx.Value(visitBudgetContextKey{}) != nil {
		return ctx
	}
	n, _ := ctx.Value(VisitBudgetContextKey{}).(int)
	if n <= 0 {
		return ctx
	}
	return context.WithValue(ctx, visitBudgetContextKey{}, &visitBudget{remaining: n})
}

// visitNode accounts one visited node of recursive descent and reports
// cancellation. A nil error means the traversal may continue.
func visitNode(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if b, ok := ctx.Value(visitBudgetContextKey{}).(*visitBudget); ok {
		if b.remaining <= 0 {
			return ErrVisitBudgetExceeded
		}
		b.remaining--
	}
	return nil
}

// isFatalError reports whether err must abort the whole evaluation instead of
// being treated as a per-branch mismatch. Cancellation, deadline and budget
// errors are fatal; ordinary selection errors keep their historic
// skip-on-error semantics.
func isFatalError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrVisitBudgetExceeded) {
		return true
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	return false
}
