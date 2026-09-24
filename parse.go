package jsonpath

import (
	"context"
	"fmt"
	"math"
	"text/scanner"

	"github.com/PaesslerAG/gval"
)

type parser struct {
	*gval.Parser
	path     path
	segments []ParsedSegment
}

func parseRootPath(ctx context.Context, gParser *gval.Parser) (r gval.Evaluable, err error) {
	p := newParser(gParser)
	return p.parse(ctx)
}

func parseCurrentPath(ctx context.Context, gParser *gval.Parser) (r gval.Evaluable, err error) {
	p := newParser(gParser)
	p.appendPlainSelector(currentElementSelector(), ParsedSegment{Kind: SegmentCurrent})
	eval, err := p.parse(ctx)
	// We always want to ensure that the machinery to collect values with paths is circumvented for current path
	return func(ctx context.Context, parameter interface{}) (interface{}, error) {
		if err != nil {
			return nil, err
		}
		value, err := eval(ctx, parameter)
		collectFullPaths := ctx.Value(CollectFullPathsContextKey{})
		if b, ok := collectFullPaths.(bool); ok && b {
			if m, ok := value.(map[string]interface{}); ok {
				for _, v := range m {
					return v, nil
				}
			}
		}
		return value, err
	}, nil
}

func newParser(p *gval.Parser) *parser {
	return &parser{Parser: p, path: plainPath{}}
}

// wrapEvaluation installs the per-evaluation visit guard (budget,
// cancellation) around the compiled path. The guard is created here for each
// invocation, so counters can never be shared between evaluations, including
// repeated evaluations of the same evaluable.
func (p *parser) wrapEvaluation(eval func(context.Context, interface{}) (interface{}, error)) gval.Evaluable {
	return func(ctx context.Context, parameter interface{}) (interface{}, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return eval(visitGuardFor(ctx), parameter)
	}
}

func (p *parser) parse(c context.Context) (r gval.Evaluable, err error) {
	err = p.parsePath(c)
	if err != nil {
		return nil, err
	}
	if sink, ok := c.Value(ParsedSegmentsContextKey{}).(*[]ParsedSegment); ok && sink != nil {
		*sink = append(*sink, p.segments...)
	}
	collectFullPaths := c.Value(CollectFullPathsContextKey{})
	if b, ok := collectFullPaths.(bool); ok && b {
		return p.wrapEvaluation(p.path.evaluateWithPaths), nil
	}
	return p.wrapEvaluation(p.path.evaluate), nil
}

func (p *parser) parsePath(c context.Context) error {
	switch p.Scan() {
	case '.':
		return p.parseSelect(c)
	case '[':
		keys, seperator, err := p.parseBracket(c)

		if err != nil {
			return err
		}

		switch seperator {
		case ':':
			if len(keys) > 3 {
				return fmt.Errorf("range query has at least the parameter [min:max:step]")
			}
			keys = append(keys, []gval.Evaluable{
				p.Const(0), p.Const(float64(math.MaxInt32)), p.Const(1)}[len(keys):]...)
			p.appendAmbiguousSelector(rangeSelector(keys[0], keys[1], keys[2]), p.sliceSegment(c, keys))
		case '?':
			if len(keys) != 1 {
				return fmt.Errorf("filter needs exactly one key")
			}
			p.appendAmbiguousSelector(filterSelector(keys[0]), ParsedSegment{Kind: SegmentFilter})
		default:
			if len(keys) == 1 {
				p.appendPlainSelector(directSelector(keys[0]), ParsedSegment{Kind: SegmentDirect, Key: constKeyValue(c, keys[0])})
			} else {
				p.appendAmbiguousSelector(multiSelector(keys), p.multiSegment(c, keys))
			}
		}
		return p.parsePath(c)
	case '(':
		return p.parseScript(c)
	default:
		p.Camouflage("jsonpath", '.', '[', '(')
		return nil
	}
}

func (p *parser) parseSelect(c context.Context) error {
	scan := p.Scan()
	switch scan {
	case scanner.Ident:
		token := p.TokenText()
		p.appendPlainSelector(directSelector(p.Const(token)), ParsedSegment{Kind: SegmentDirect, Key: token})
		return p.parsePath(c)
	case '.':
		p.appendAmbiguousSelector(mapperSelector(), ParsedSegment{Kind: SegmentRecursive})
		return p.parseMapper(c)
	case '*':
		p.appendAmbiguousSelector(starSelector(), ParsedSegment{Kind: SegmentStar})
		return p.parsePath(c)
	default:
		return p.Expected("JSON select", scanner.Ident, '.', '*')
	}
}

func (p *parser) parseBracket(c context.Context) (keys []gval.Evaluable, seperator rune, err error) {
	for {
		scan := p.Scan()
		skipScan := false
		switch scan {
		case '?':
			skipScan = true
		case ':':
			i := float64(0)
			if len(keys) == 1 {
				i = math.MaxInt32
			}
			keys = append(keys, p.Const(i))
			skipScan = true
		case '*':
			if p.Scan() != ']' {
				return nil, 0, p.Expected("JSON bracket star", ']')
			}
			return []gval.Evaluable{}, 0, nil
		case ']':
			if seperator == ':' {
				skipScan = true
				break
			}
			fallthrough
		default:
			p.Camouflage("jsonpath brackets")
			key, err := p.ParseExpression(c)
			if err != nil {
				return nil, 0, err
			}
			keys = append(keys, key)
		}
		if !skipScan {
			scan = p.Scan()
		}
		if seperator == 0 {
			seperator = scan
		}
		switch scan {
		case ':', ',':
		case ']':
			return
		case '?':
			if len(keys) != 0 {
				return nil, 0, p.Expected("JSON filter", ']')
			}
		default:
			return nil, 0, p.Expected("JSON bracket separator", ':', ',')
		}
		if seperator != scan {
			return nil, 0, fmt.Errorf("mixed %v and %v in JSON bracket", seperator, scan)
		}
	}
}

func (p *parser) parseMapper(c context.Context) error {
	scan := p.Scan()
	switch scan {
	case scanner.Ident:
		token := p.TokenText()
		p.appendPlainSelector(directSelector(p.Const(token)), ParsedSegment{Kind: SegmentDirect, Key: token})
	case '[':
		keys, seperator, err := p.parseBracket(c)

		if err != nil {
			return err
		}
		switch seperator {
		case ':':
			return fmt.Errorf("mapper can not be combined with range query")
		case '?':
			if len(keys) != 1 {
				return fmt.Errorf("filter needs exactly one key")
			}
			p.appendAmbiguousSelector(filterSelector(keys[0]), ParsedSegment{Kind: SegmentFilter})
		default:
			p.appendAmbiguousSelector(multiSelector(keys), p.multiSegment(c, keys))
		}
	case '*':
		p.appendAmbiguousSelector(starSelector(), ParsedSegment{Kind: SegmentStar})
	case '(':
		return p.parseScript(c)
	default:
		return p.Expected("JSON mapper", '[', scanner.Ident, '*')
	}
	return p.parsePath(c)
}

func (p *parser) parseScript(c context.Context) error {
	script, err := p.ParseExpression(c)
	if err != nil {
		return err
	}
	if p.Scan() != ')' {
		return p.Expected("jsonpath script", ')')
	}
	p.appendPlainSelector(newScript(script), ParsedSegment{Kind: SegmentScript})
	return p.parsePath(c)
}

func (p *parser) appendPlainSelector(next plainSelector, segment ParsedSegment) {
	p.segments = append(p.segments, segment)
	p.path = p.path.withPlainSelector(next)
}

func (p *parser) appendAmbiguousSelector(next ambiguousSelector, segment ParsedSegment) {
	p.segments = append(p.segments, segment)
	p.path = p.path.withAmbiguousSelector(next)
}

func (p *parser) multiSegment(c context.Context, keys []gval.Evaluable) ParsedSegment {
	if len(keys) == 0 {
		return ParsedSegment{Kind: SegmentStar}
	}
	segment := ParsedSegment{Kind: SegmentMulti}
	for _, key := range keys {
		segment.Keys = append(segment.Keys, constKeyValue(c, key))
	}
	return segment
}

func (p *parser) sliceSegment(c context.Context, keys []gval.Evaluable) ParsedSegment {
	bounds := SliceBounds{}
	bounds.Min, bounds.MinConstant = constKeyValueOK(c, keys[0])
	bounds.Max, bounds.MaxConstant = constKeyValueOK(c, keys[1])
	bounds.Step, bounds.StepConstant = constKeyValueOK(c, keys[2])
	return ParsedSegment{Kind: SegmentSlice, Slice: &bounds}
}

// constKeyValue evaluates a bracket key expression against a nil parameter.
// A literal key evaluates without touching the parameter; any dynamic key
// (@.x, expressions, parameters) fails and is reported as nil so that a
// restricted reference model can reject it explicitly.
func constKeyValue(c context.Context, key gval.Evaluable) interface{} {
	v, ok := constKeyValueOK(c, key)
	if !ok {
		return nil
	}
	return v
}

func constKeyValueOK(c context.Context, key gval.Evaluable) (interface{}, bool) {
	v, err := key(currentContext(c, nil), nil)
	if err != nil {
		return nil, false
	}
	switch v.(type) {
	case string, float64, bool, nil:
		return v, true
	default:
		return v, false
	}
}
