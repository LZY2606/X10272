package jsonpath

import (
	"bytes"
	"context"
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

type ambiguousMatcher func(key, v interface{}) error

func (p plainPath) evaluate(ctx context.Context, parameter interface{}) (interface{}, error) {
	_, value, err := p.evaluatePath(ctx, parameter, parameter)
	return value, err
}

func (p plainPath) evaluateWithPaths(ctx context.Context, parameter interface{}) (interface{}, error) {
	keys, value, err := p.evaluatePath(ctx, parameter, parameter)
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
		if err != nil {
			if isFatalError(err) {
				return err
			}
			return nil
		}
		if b, ok := collectFullPaths.(bool); ok && b {
			keys = append(ks, k)
		}
		return match(keys, res)
	}
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

func (p plainPath) visitMatchs(ctx context.Context, r interface{}, visit pathMatcher) error {
	keys, res, err := p.evaluatePath(ctx, r, r)
	if err != nil {
		if isFatalError(err) {
			return err
		}
		return nil
	}
	return visit(keys, res)
}

type ambiguousPath struct {
	parent path
	branch ambiguousSelector
	ending plainPath
}

func (p *ambiguousPath) evaluate(ctx context.Context, parameter interface{}) (interface{}, error) {
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
		// copy keys: the slice may be reused by the caller for the next match,
		// so retaining it would leak state between matches of one evaluation.
		ks := make([]interface{}, len(keys)+1)
		copy(ks, keys)
		ks[len(keys)] = key
		return m(ks, match)
	}
}

func convertPath(segments []interface{}) []interface{} {
	paths := []interface{}{}
	sCount := len(segments)

	for i, sRaw := range segments {
		if s, ok := sRaw.([]interface{}); ok {
			// An empty nested segment (recursive descent's self match carries
			// no keys) contributes nothing and cannot be rotated.
			if len(s) == 0 {
				continue
			}
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
