package jsonpath

import (
	"bytes"
	"context"
	"fmt"
)

// Exported for testing purposes
type CollectFullPathsContextKey struct{}

type path interface {
	evaluate(c context.Context, parameter interface{}) (interface{}, error)
	evaluateWithPaths(c context.Context, parameter interface{}) (interface{}, error)
	visitMatchs(c context.Context, r interface{}, visit pathMatcher) error
	withPlainSelector(plainSelector) path
	withAmbiguousSelector(ambiguousSelector) path
}

type plainPath []plainSelector

// ambiguousMatcher receives one wildcard match. It returns an error when the
// match must abort the whole evaluation (for example an exhausted
// recursive-descent visit budget or a cancelled context). Errors while
// resolving a single match stay swallowed inside plainPath.matcher,
// preserving the per-selector failure isolation.
type ambiguousMatcher func(key, v interface{}) error

func (p plainPath) evaluate(ctx context.Context, root interface{}) (interface{}, error) {
	_, value, err := p.evaluatePath(ctx, root, root)
	return value, err
}

func (p plainPath) evaluateWithPaths(ctx context.Context, root interface{}) (interface{}, error) {
	keys, value, err := p.evaluatePath(ctx, root, root)
	m := map[string]interface{}{}
	m[toJSONPath(keys)] = value
	return m, err
}

func (p plainPath) evaluatePath(ctx context.Context, root, value interface{}) ([]interface{}, interface{}, error) {
	keys := []interface{}{}
	for _, sel := range p {
		k, v, err := sel(ctx, root, value)
		if err != nil {
			return nil, nil, err
		}
		if k != nil {
			keys = append(keys, k)
		}
		value = v
	}
	return keys, value, nil
}

func (p plainPath) matcher(ctx context.Context, r interface{}, match ambiguousMatcher) ambiguousMatcher {
	if len(p) == 0 {
		return match
	}
	return func(k, v interface{}) error {
		keys := k
		collectFullPaths := ctx.Value(CollectFullPathsContextKey{})
		ks, res, err := p.evaluatePath(ctx, r, v)
		if b, ok := collectFullPaths.(bool); ok && b {
			keys = append(ks, k)
		}
		if err == nil {
			return match(keys, res)
		}
		return nil
	}
}

func (p plainPath) visitMatchs(ctx context.Context, r interface{}, visit pathMatcher) error {
	keys, res, err := p.evaluatePath(ctx, r, r)
	if err == nil {
		return visit(keys, res)
	}
	return nil
}

func (p plainPath) withPlainSelector(selector plainSelector) path {
	return append(p, selector)
}
func (p plainPath) withAmbiguousSelector(selector ambiguousSelector) path {
	return &ambiguousPath{
		parent: p,
		branch: selector,
	}
}

type ambiguousPath struct {
	parent path
	branch ambiguousSelector
	ending plainPath
}

func (p *ambiguousPath) evaluate(ctx context.Context, parameter interface{}) (interface{}, error) {
	ctx = withVisitBudget(ctx)
	matchs := []interface{}{}
	err := p.visitMatchs(ctx, parameter, func(keys []interface{}, match interface{}) error {
		matchs = append(matchs, match)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return matchs, nil
}

func (p *ambiguousPath) evaluateWithPaths(ctx context.Context, parameter interface{}) (interface{}, error) {
	ctx = withVisitBudget(ctx)
	m := map[string]interface{}{}
	err := p.visitMatchs(ctx, parameter, func(keys []interface{}, match interface{}) error {
		m[toJSONPath(convertPath(keys))] = match
		return nil
	})
	if err != nil {
		return nil, err
	}
	return m, nil
}

func (p *ambiguousPath) visitMatchs(ctx context.Context, r interface{}, visit pathMatcher) error {
	return p.parent.visitMatchs(ctx, r, func(keys []interface{}, v interface{}) error {
		return p.branch(ctx, r, v, p.ending.matcher(ctx, r, visit.matcher(keys)))
	})
}

func (p *ambiguousPath) branchMatcher(ctx context.Context, r interface{}, m ambiguousMatcher) ambiguousMatcher {
	return func(k, v interface{}) error {
		return p.branch(ctx, r, v, m)
	}
}

func (p *ambiguousPath) withPlainSelector(selector plainSelector) path {
	p.ending = append(p.ending, selector)
	return p
}
func (p *ambiguousPath) withAmbiguousSelector(selector ambiguousSelector) path {
	return &ambiguousPath{
		parent: p,
		branch: selector,
	}
}

type pathMatcher func(keys []interface{}, match interface{}) error

func (m pathMatcher) matcher(keys []interface{}) ambiguousMatcher {
	return func(key, match interface{}) error {
		return m(append(keys, key), match)
	}
}

func convertPath(segments []interface{}) []interface{} {
	paths := []interface{}{}
	sCount := len(segments)

	for i, sRaw := range segments {
		if s, ok := sRaw.([]interface{}); ok {
			liIndex := len(s) - 1

			if (liIndex >= 0 && i == sCount-1) || (i > 0 && liIndex < sCount) {
				s = convertPath(append([]interface{}{s[liIndex]}, s[:liIndex]...))
			}

			paths = append(paths, convertPath(s)...)
		} else {
			paths = append(paths, sRaw)
		}
	}

	return paths
}

func toJSONPath(segments []interface{}) string {
	sb := bytes.Buffer{}
	sb.WriteString("$")
	quoteWildcardValues(&sb, convertPath(segments))

	return sb.String()
}

// VisitLimitContextKey is an optional diagnostic hook that bounds the number of
// nodes a single evaluation may visit during recursive descent (".."). Put a
// positive int into the evaluation context to enable it; zero or no value means
// unbounded. The counter is created fresh for every evaluation, so a budget
// failure can never leak into subsequent calls of a reused evaluable.
//
// The check runs on the same production mapper path as ordinary recursive
// descent; it is not a test-only copy of the traversal algorithm.
type VisitLimitContextKey struct{}

type visitBudgetContextKeyType struct{}

var visitBudgetContextKey = visitBudgetContextKeyType{}

type visitBudget struct {
	limit int
	used  int
}

func (b *visitBudget) visit() error {
	if b == nil || b.limit <= 0 {
		return nil
	}
	b.used++
	if b.used > b.limit {
		return fmt.Errorf("jsonpath: recursive descent visit budget of %d exceeded", b.limit)
	}
	return nil
}

// withVisitBudget installs a fresh budget for one top-level evaluation.
// Nested evaluations reuse the existing budget instead of resetting it.
func withVisitBudget(ctx context.Context) context.Context {
	if ctx.Value(visitBudgetContextKey) != nil {
		return ctx
	}
	limit, _ := ctx.Value(VisitLimitContextKey{}).(int)
	return context.WithValue(ctx, visitBudgetContextKey, &visitBudget{limit: limit})
}

func visitBudgetFrom(ctx context.Context) *visitBudget {
	b, _ := ctx.Value(visitBudgetContextKey).(*visitBudget)
	return b
}
