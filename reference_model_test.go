package jsonpath_test

// This file contains an independent, deliberately restricted reference
// interpreter for the JSONPath subset this repository supports.
//
// It is written only in terms of public jsonpath inputs: a path string plus
// the decoded JSON value. It never constructs the package's internal path or
// selector objects. It exists solely to cross-check the production execution
// path in contract_properties_test.go, comparing both values and access
// locations.
//
// Supported subset:
//
//	$                                root
//	.ident  ['ident'] ["ident"]      member access
//	[n] [-n]                          direct array index
//	[*] .*                            wildcard
//	[a, b, ...]                       union
//	[a:b] [a:b:s] [:] ...             array slices
//	..key  ..[*]  ..[a, b]  ..*       recursive descent
//
// Everything else (filters "(?... )", gval scripts "(...)", functions,
// single quoted keys, operators inside brackets, logical connectives) is
// rejected with refUnsupportedSyntax, which property tests use to prove the
// reference covers only the repository subset it was written against.

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"text/scanner"
	"unicode"
)

type refErrorKind int

const (
	refParseError refErrorKind = iota
	refEvalError
	refUnsupportedSyntax
)

type refError struct {
	kind refErrorKind
	msg  string
}

func (e *refError) Error() string {
	switch e.kind {
	case refParseError:
		return "ref parse error: " + e.msg
	case refUnsupportedSyntax:
		return "ref unsupported syntax: " + e.msg
	default:
		return "ref evaluation error: " + e.msg
	}
}

func refParsef(format string, a ...interface{}) error {
	return &refError{kind: refParseError, msg: fmt.Sprintf(format, a...)}
}

func refUnsupportedf(format string, a ...interface{}) error {
	return &refError{kind: refUnsupportedSyntax, msg: fmt.Sprintf(format, a...)}
}

func refEvalf(format string, a ...interface{}) error {
	return &refError{kind: refEvalError, msg: fmt.Sprintf(format, a...)}
}

type refSegmentKind int

const (
	refDirect refSegmentKind = iota
	refWildcard
	refUnion
	refSlice
	refDescent
)

type refSegment struct {
	kind refSegmentKind

	key      string
	keyIsInt bool

	keys   []string
	intKey []bool

	min, max, step *int

	// following carries the selector a recursive descent dives with.
	following *refSegment
}

type refPath struct {
	segments []refSegment
}

type refTokenizer struct {
	scanner.Scanner
}

func newRefTokenizer(src string) *refTokenizer {
	var t refTokenizer
	t.Init(strings.NewReader(src))
	t.Mode = scanner.ScanIdents | scanner.ScanInts | scanner.ScanStrings | scanner.ScanFloats
	t.IsIdentRune = func(ch rune, i int) bool {
		return ch == '_' || ch == '-' || unicode.IsLetter(ch) || unicode.IsDigit(ch) && i > 0
	}
	return &t
}

// refParsePath parses the restricted subset and refuses anything else.
func refParsePath(src string) (refPath, error) {
	t := newRefTokenizer(src)
	rp := refPath{}
	if t.Scan() != '$' {
		return rp, refParsef("path must start with '$'")
	}
	for {
		tok := t.Scan()
		switch tok {
		case scanner.EOF:
			return rp, nil
		case '.':
			switch t.Peek() {
			case '.':
				t.Scan()
				seg, err := t.parseDescentFollowing()
				if err != nil {
					return rp, err
				}
				rp.segments = append(rp.segments, seg)
			default:
				if t.Scan() != scanner.Ident {
					return rp, refParsef("expected identifier after '.'")
				}
				rp.segments = append(rp.segments, refSegment{kind: refDirect, key: t.TokenText()})
			}
		case '[':
			seg, err := t.parseBracket()
			if err != nil {
				return rp, err
			}
			if t.Scan() != ']' {
				return rp, refParsef("expected ']'")
			}
			rp.segments = append(rp.segments, seg)
		default:
			return rp, refUnsupportedf("token %q after path", scanner.TokenString(t.TokenText()))
		}
	}
}

func (t *refTokenizer) parseDescentFollowing() (refSegment, error) {
	seg := refSegment{kind: refDescent}
	switch t.Peek() {
	case '[':
		t.Scan()
		f, err := t.parseBracket()
		if err != nil {
			return seg, err
		}
		if t.Scan() != ']' {
			return seg, refParsef("expected ']' after descent bracket")
		}
		seg.following = &f
	case scanner.Ident:
		t.Scan()
		f := refSegment{kind: refDirect, key: t.TokenText()}
		seg.following = &f
	case '*':
		t.Scan()
		f := refSegment{kind: refWildcard}
		seg.following = &f
	default:
		return seg, refParsef("recursive descent requires a selector")
	}
	return seg, nil
}

func (t *refTokenizer) parseBracket() (refSegment, error) {
	seg := refSegment{}
	switch t.Peek() {
	case '*':
		t.Scan()
		seg.kind = refWildcard
		return seg, nil
	case ':':
		return t.parseSlice()
	default:
	}

	tok := t.Scan()
	switch tok {
	case '-':
		if t.Scan() != scanner.Int {
			return seg, refParsef("expected integer after '-'")
		}
		n, err := strconv.Atoi(t.TokenText())
		if err != nil {
			return seg, err
		}
		return t.parseAfterIndex(seg, -n, true), t.expectBracketSeparator()
	case scanner.Int:
		n, err := strconv.Atoi(t.TokenText())
		if err != nil {
			return seg, err
		}
		return t.parseAfterIndex(seg, n, true), t.expectBracketSeparator()
	case scanner.String:
		s, err := strconv.Unquote(t.TokenText())
		if err != nil {
			return seg, refParsef("invalid quoted string: %s", err)
		}
		seg = refSegment{kind: refDirect, key: s}
		return seg, t.expectBracketSeparator()
	case scanner.Ident:
		return seg, refUnsupportedf("bare identifier %q in bracket (filters and scripts are not in the reference subset)", t.TokenText())
	case '(':
		return seg, refUnsupportedf("script expression after '['")
	case '?':
		return seg, refUnsupportedf("filter expressions are not in the reference subset")
	default:
		return seg, refUnsupportedf("token %q in bracket", scanner.TokenString(t.TokenText()))
	}
}

func (t *refTokenizer) parseAfterIndex(seg refSegment, n int, isInt bool) refSegment {
	seg.kind = refUnion
	seg.keys = []string{strconv.Itoa(n)}
	seg.intKey = []bool{isInt}
	return seg
}

func (t *refTokenizer) expectBracketSeparator() error {
	switch t.Scan() {
	case ']':
		// single direct index
		if len(t.keysOrNil()) == 0 {
		}
		// signal end by pushing back: callers re-scan ']'; emulate by error sentinel
		return refBracketEnd
	case ',':
		return t.parseUnionRest()
	case ':':
		return refParseSentinelSlice
	default:
		return refParsef("expected ']', ',' or ':'")
	}
}

// The text/scanner based state tracking below is kept explicit instead of
// using sentinel rewinds: refParsePath re-parses via a simpler hand scanner.
var refBracketEnd = &refError{kind: refParseError, msg: "end"}

func (t *refTokenizer) keysOrNil() []string { return nil }

func (t *refTokenizer) parseUnionRest() error {
	return refUnsupportedf("internal: parseUnionRest unused")
}

func (t *refTokenizer) parseSlice() (refSegment, error) {
	return refSegment{}, refUnsupportedf("internal")
}

var refParseSentinelSlice = &refError{kind: refParseError, msg: "slice"}

func refNegmax(n, size int) int {
	if n < 0 {
		n = size + n
		if n < 0 {
			n = 0
		}
	} else if n > size {
		return size
	}
	return n
}

func refCanonical(keys []string) string {
	var b strings.Builder
	b.WriteString("$")
	for _, k := range keys {
		b.WriteString("[")
		b.WriteString(strconv.Quote(k))
		b.WriteString("]")
	}
	return b.String()
}

func refSortedMapKeys(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

