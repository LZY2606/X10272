package jsonpath_test

// restrictedRef is an intentionally small, independently written JSONPath
// interpreter used only by the selector-composition regression suite.
//
// It is NOT a copy of the production evaluator: it re-parses the query with
// its own tokenizer and walks plain decoded values (map[string]interface{}
// and []interface{}) with its own recursion. It only covers the subset the
// repository supports without additional gval modules:
//
//	$                        root
//	.ident                   dot child
//	[<int>]                  array index (negative indices allowed)
//	["string"]               quoted object key
//	[a, b, ...]              union
//	.* / [*]                 wildcard
//	..                       recursive descent followed by one selector
//	[min:max] / [min:max:step]
//
// Everything else (filters, scripts, single quotes, functions, operators,
// placeholder objects, current-node paths, ...) is rejected with
// errRefRejected so tests can prove that the reference never silently guesses
// semantics outside the shared subset.

import (
	"fmt"
	"strconv"
	"strings"
)

var errRefRejected = fmt.Errorf("restricted reference: syntax outside supported subset")

type refSegment struct {
	kind  byte // 'd' direct, 's' star, 'r' recursive descent, 'u' union, 'c' slice
	key   string
	keys  []string
	lo    *int
	hi    *int
	st    *int
	after byte // selector following a recursive descent: d/s/u/c
	akey  string
	akeys []string
	alo   *int
	ahi   *int
	ast   *int
}

type refParser struct {
	q   string
	pos int
}

func refParse(query string) ([]refSegment, error) {
	if query == "" || query[0] != '$' {
		return nil, fmt.Errorf("%w: query must start with $: %q", errRefRejected, query)
	}
	rp := &refParser{q: query, pos: 1}
	var segs []refSegment
	var descent *refSegment
	flush := func() {
		if descent != nil {
			segs = append(segs, *descent)
			descent = nil
		}
	}
	for rp.pos < len(rp.q) {
		switch rp.q[rp.pos] {
		case '.':
			rp.pos++
			if rp.pos < len(rp.q) && rp.q[rp.pos] == '.' {
				flush()
				rp.pos++
				d := refSegment{kind: 'r'}
				descent = &d
				if rp.pos >= len(rp.q) {
					return nil, fmt.Errorf("%w: dangling recursive descent: %q", errRefRejected, query)
				}
				continue
			}
			name, err := rp.ident()
			if err != nil {
				return nil, err
			}
			if descent != nil {
				descent.after = 'd'
				descent.akey = name
				flush()
				continue
			}
			segs = append(segs, refSegment{kind: 'd', key: name})
		case '*':
			if descent != nil {
				descent.after = 's'
				flush()
				rp.pos++
				continue
			}
			return nil, fmt.Errorf("%w: stray *: %q", errRefRejected, query)
		case '[':
			seg, err := rp.bracket(query, descent != nil)
			if err != nil {
				return nil, err
			}
			if descent != nil {
				descent.after = seg.kind
				descent.akey = seg.key
				descent.akeys = seg.keys
				descent.alo = seg.lo
				descent.ahi = seg.hi
				descent.ast = seg.st
				flush()
			} else {
				segs = append(segs, seg)
			}
		default:
			return nil, fmt.Errorf("%w: unexpected %q at %d: %q", errRefRejected, string(rp.q[rp.pos]), rp.pos, query)
		}
	}
	flush()
	return segs, nil
}

func (rp *refParser) ident() (string, error) {
	start := rp.pos
	for rp.pos < len(rp.q) {
		b := rp.q[rp.pos]
		if b == '.' || b == '[' {
			break
		}
		rp.pos++
	}
	name := rp.q[start:rp.pos]
	if name == "" {
		return "", fmt.Errorf("%w: empty identifier", errRefRejected)
	}
	for _, b := range []byte(name) {
		if !(b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '_') {
			return "", fmt.Errorf("%w: unsupported identifier %q", errRefRejected, name)
		}
	}
	return name, nil
}

// bracket parses one [...] group after the opening '[' has been peeked.
func (rp *refParser) bracket(query string, afterDescent bool) (refSegment, error) {
	rp.pos++ // consume [
	contentStart := rp.pos
	depth := 1
	for rp.pos < len(rp.q) && depth > 0 {
		switch rp.q[rp.pos] {
		case '[':
			depth++
		case ']':
			depth--
		case '?', '(', '\'', '{', '@', '=', '<', '>', '!', '~':
			return refSegment{}, fmt.Errorf("%w: unsupported bracket feature %q: %q", errRefRejected, string(rp.q[rp.pos]), query)
		}
		if depth > 0 {
			rp.pos++
		}
	}
	if depth != 0 {
		return refSegment{}, fmt.Errorf("%w: unclosed bracket: %q", errRefRejected, query)
	}
	inside := rp.q[contentStart:rp.pos]
	rp.pos++ // consume ]

	parts := splitTopLevel(inside, ':')
	if len(parts) > 1 {
		if len(parts) > 3 || strings.Contains(inside, ",") {
			return refSegment{}, fmt.Errorf("%w: malformed slice %q", errRefRejected, inside)
		}
		seg := refSegment{kind: 'c'}
		if err := bindSliceParts(&seg, parts); err != nil {
			return refSegment{}, err
		}
		return seg, nil
	}
	keys := splitTopLevel(inside, ',')
	if len(keys) > 1 {
		seg := refSegment{kind: 'u'}
		for _, k := range keys {
			kk, err := rp.bracketKey(strings.TrimSpace(k), query)
			if err != nil {
				return refSegment{}, err
			}
			seg.keys = append(seg.keys, kk)
		}
		return seg, nil
	}
	if inside == "*" {
		return refSegment{kind: 's'}, nil
	}
	k, err := rp.bracketKey(strings.TrimSpace(inside), query)
	if err != nil {
		return refSegment{}, err
	}
	return refSegment{kind: 'd', key: k}, nil
}

func (rp *refParser) bracketKey(token, query string) (string, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return "", fmt.Errorf("%w: empty bracket key: %q", errRefRejected, query)
	}
	if token[0] == '"' {
		s, err := strconv.Unquote(token)
		if err != nil {
			return "", fmt.Errorf("%w: %v", errRefRejected, err)
		}
		return s, nil
	}
	if _, err := strconv.Atoi(token); err == nil {
		return token, nil
	}
	for _, b := range []byte(token) {
		if !(b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '_' || b == '-') {
			return "", fmt.Errorf("%w: unsupported bracket key %q", errRefRejected, token)
		}
	}
	return token, nil
}

func splitTopLevel(s string, sep byte) []string {
	var out []string
	depth := 0
	inStr := false
	last := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"':
			if i == 0 || s[i-1] != '\\' {
				inStr = !inStr
			}
		case '[':
			if !inStr {
				depth++
			}
		case ']':
			if !inStr {
				depth--
			}
		default:
			if s[i] == sep && depth == 0 && !inStr {
				out = append(out, s[last:i])
				last = i + 1
			}
		}
	}
	return append(out, s[last:])
}

func bindSliceParts(seg *refSegment, parts []string) error {
	parseOne := func(s string) (*int, error) {
		s = strings.TrimSpace(s)
		if s == "" {
			return nil, nil
		}
		n, err := strconv.Atoi(s)
		if err != nil {
			return nil, fmt.Errorf("%w: non-integer slice bound %q", errRefRejected, s)
		}
		return &n, nil
	}
	var err error
	if seg.lo, err = parseOne(parts[0]); err != nil {
		return err
	}
	if seg.hi, err = parseOne(parts[1]); err != nil {
		return err
	}
	if len(parts) == 3 {
		if seg.st, err = parseOne(parts[2]); err != nil {
			return err
		}
	}
	return nil
}
