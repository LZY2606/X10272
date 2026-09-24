package jsonpath_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Restricted reference interpreter for "jsonpath: selector composition,
// placeholder binding and failure isolation".
//
// The reference model is independent from the production parser/evaluator and
// only covers the selector subset actually supported by this repository:
//
//	$                         root
//	.name                     object child (plain selector)
//	[n], [-n]                 array index, negative counted from the end
//	["name"]                  quoted object child
//	[*], .*                   wildcard (ambiguous selector)
//	[k1,k2,...]               union (ambiguous selector)
//	[min:max], [min:max:step] array slice (ambiguous selector)
//	..name, ..*, ..[...]      recursive descent (ambiguous selector)
//
// Every other construct (filters, scripts, placeholders, non-integer
// bracketed numbers, mixed separators, ...) is rejected explicitly with
// errRefUnsupported. Recursive descent is executed under a visit budget and
// object members are visited in sorted key order, so traversal order is
// reproducible.
var (
	errRefUnsupported = errors.New("syntax outside the supported reference subset")
	errRefBudget      = errors.New("recursive descent visit budget exhausted")
)

// refDefaultBudget is used by all cross-checks that compare against the
// production evaluator; it is intentionally far above any fixture size.
const refDefaultBudget = 100000

type refKey struct {
	isIndex bool
	index   int
	name    string
}

func (k refKey) asObjectKey() string {
	if k.isIndex {
		return strconv.Itoa(k.index)
	}
	return k.name
}

type refSegKind int

const (
	refChild refSegKind = iota // exactly one value, errors are visible
	refUnion                   // zero or more values, per-key failures dropped
	refSlice                   // zero or more array elements
	refWildcard                // all children of a container
	refRecurse                 // recursive descent into a sub selector
)

type refSeg struct {
	kind refSegKind
	key  refKey    // refChild
	keys []refKey  // refUnion
	min  *int      // refSlice
	max  *int      // refSlice
	step *int      // refSlice
	sub  *refSeg   // refRecurse: refChild, refUnion or refWildcard
}

func (s refSeg) ambiguous() bool {
	return s.kind != refChild
}

// refMatch is one access position together with the value found there.
type refMatch struct {
	keys  []string
	value interface{}
}

// path renders an access position in the repository's GetWithPaths notation.
func (m refMatch) path() string {
	sb := strings.Builder{}
	sb.WriteString("$")
	for _, k := range m.keys {
		sb.WriteString("[")
		sb.WriteString(strconv.Quote(k))
		sb.WriteString("]")
	}
	return sb.String()
}

func appendRefKey(keys []string, k string) []string {
	out := make([]string, 0, len(keys)+1)
	out = append(out, keys...)
	out = append(out, k)
	return out
}

// ---------------------------------------------------------------------------
// Parser for the restricted reference grammar
// ---------------------------------------------------------------------------

type refParser struct {
	input string
	pos   int
}

func parseReferencePath(path string) ([]refSeg, error) {
	p := &refParser{input: path}
	if !p.consume('$') {
		return nil, fmt.Errorf("%w: path must start with $", errRefUnsupported)
	}
	var segs []refSeg
	for !p.eof() {
		seg, err := p.parseSegment()
		if err != nil {
			return nil, err
		}
		segs = append(segs, seg)
	}
	return segs, nil
}

func (p *refParser) eof() bool {
	return p.pos >= len(p.input)
}

func (p *refParser) peek() byte {
	if p.eof() {
		return 0
	}
	return p.input[p.pos]
}

func (p *refParser) consume(c byte) bool {
	if p.peek() == c {
		p.pos++
		return true
	}
	return false
}

func (p *refParser) skipSpaces() {
	for !p.eof() && (p.peek() == ' ' || p.peek() == '\t') {
		p.pos++
	}
}

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdentPart(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9')
}

func (p *refParser) parseIdent() (string, error) {
	start := p.pos
	if !isIdentStart(p.peek()) {
		return "", fmt.Errorf("%w: expected identifier at position %d", errRefUnsupported, p.pos)
	}
	for !p.eof() && isIdentPart(p.peek()) {
		p.pos++
	}
	return p.input[start:p.pos], nil
}

func (p *refParser) parseInt() (int, error) {
	start := p.pos
	if p.peek() == '-' {
		p.pos++
	}
	if p.peek() < '0' || p.peek() > '9' {
		return 0, fmt.Errorf("%w: expected integer at position %d", errRefUnsupported, start)
	}
	for !p.eof() && p.peek() >= '0' && p.peek() <= '9' {
		p.pos++
	}
	n, err := strconv.Atoi(p.input[start:p.pos])
	if err != nil {
		return 0, fmt.Errorf("%w: %v", errRefUnsupported, err)
	}
	return n, nil
}

func (p *refParser) parseQuoted() (string, error) {
	start := p.pos
	if !p.consume('"') {
		return "", fmt.Errorf("%w: expected quoted string", errRefUnsupported)
	}
	for !p.eof() && p.peek() != '"' {
		if p.peek() == '\\' {
			p.pos++
			if p.eof() {
				break
			}
		}
		p.pos++
	}
	if !p.consume('"') {
		return "", fmt.Errorf("%w: unterminated quoted string", errRefUnsupported)
	}
	s, err := strconv.Unquote(p.input[start:p.pos])
	if err != nil {
		return "", fmt.Errorf("%w: %v", errRefUnsupported, err)
	}
	return s, nil
}

func (p *refParser) parseSegment() (refSeg, error) {
	if p.pos+1 < len(p.input) && p.input[p.pos] == '.' && p.input[p.pos+1] == '.' {
		p.pos += 2
		sub, err := p.parseRecurseTarget()
		if err != nil {
			return refSeg{}, err
		}
		return refSeg{kind: refRecurse, sub: &sub}, nil
	}
	if p.consume('.') {
		if p.consume('*') {
			return refSeg{kind: refWildcard}, nil
		}
		ident, err := p.parseIdent()
		if err != nil {
			return refSeg{}, err
		}
		return refSeg{kind: refChild, key: refKey{name: ident}}, nil
	}
	if p.consume('[') {
		return p.parseBracket()
	}
	return refSeg{}, fmt.Errorf("%w: unexpected %q at position %d", errRefUnsupported, p.peek(), p.pos)
}

func (p *refParser) parseRecurseTarget() (refSeg, error) {
	if p.consume('*') {
		return refSeg{kind: refWildcard}, nil
	}
	if p.peek() == '[' {
		p.pos++
		seg, err := p.parseBracket()
		if err != nil {
			return refSeg{}, err
		}
		if seg.kind == refSlice {
			return refSeg{}, fmt.Errorf("%w: mapper can not be combined with range query", errRefUnsupported)
		}
		return seg, nil
	}
	ident, err := p.parseIdent()
	if err != nil {
		return refSeg{}, err
	}
	return refSeg{kind: refChild, key: refKey{name: ident}}, nil
}

type refBracketPart struct {
	empty bool
	num   *int
	str   *string
}

func (p *refParser) parseBracket() (refSeg, error) {
	p.skipSpaces()
	if p.consume('*') {
		p.skipSpaces()
		if !p.consume(']') {
			return refSeg{}, fmt.Errorf("%w: [* must be closed with ]", errRefUnsupported)
		}
		return refSeg{kind: refWildcard}, nil
	}

	var parts []refBracketPart
	var seps []byte
	for {
		p.skipSpaces()
		part := refBracketPart{}
		switch c := p.peek(); {
		case c == '-' || (c >= '0' && c <= '9'):
			n, err := p.parseInt()
			if err != nil {
				return refSeg{}, err
			}
			part.num = &n
		case c == '"':
			s, err := p.parseQuoted()
			if err != nil {
				return refSeg{}, err
			}
			part.str = &s
		default:
			part.empty = true
		}
		parts = append(parts, part)
		p.skipSpaces()
		c := p.peek()
		if c == ']' {
			p.pos++
			break
		}
		if c != ',' && c != ':' {
			return refSeg{}, fmt.Errorf("%w: expected comma, colon or ] at position %d", errRefUnsupported, p.pos)
		}
		seps = append(seps, c)
		p.pos++
	}

	for _, s := range seps {
		if s != seps[0] {
			return refSeg{}, fmt.Errorf("%w: mixed separators in JSON bracket", errRefUnsupported)
		}
	}

	if len(seps) == 0 {
		part := parts[0]
		switch {
		case part.num != nil:
			return refSeg{kind: refChild, key: refKey{isIndex: true, index: *part.num}}, nil
		case part.str != nil:
			return refSeg{kind: refChild, key: refKey{name: *part.str}}, nil
		default:
			return refSeg{}, fmt.Errorf("%w: empty JSON bracket", errRefUnsupported)
		}
	}

	if seps[0] == ',' {
		keys := make([]refKey, 0, len(parts))
		for _, part := range parts {
			switch {
			case part.num != nil:
				keys = append(keys, refKey{isIndex: true, index: *part.num})
			case part.str != nil:
				keys = append(keys, refKey{name: *part.str})
			default:
				return refSeg{}, fmt.Errorf("%w: empty union member", errRefUnsupported)
			}
		}
		return refSeg{kind: refUnion, keys: keys}, nil
	}

	if len(parts) > 3 {
		return refSeg{}, fmt.Errorf("%w: range query has at most [min:max:step]", errRefUnsupported)
	}
	seg := refSeg{kind: refSlice}
	for i, part := range parts {
		if part.empty {
			continue
		}
		if part.num == nil {
			return refSeg{}, fmt.Errorf("%w: slice bounds must be integers", errRefUnsupported)
		}
		switch i {
		case 0:
			seg.min = part.num
		case 1:
			seg.max = part.num
		case 2:
			seg.step = part.num
		}
	}
	return seg, nil
}

// ---------------------------------------------------------------------------
// Evaluator of the restricted reference grammar
// ---------------------------------------------------------------------------

// refEvaluate executes the parsed segments against a JSON document.
//
// The error model mirrors the production path machinery:
//   - a path made exclusively of plain (child) selectors propagates the
//     first selection error;
//   - a path containing any ambiguous selector never reports an error,
//     because the production matcher drops branches whose plain suffix
//     fails.
//
// Recursive descent consumes the visit budget and aborts with errRefBudget;
// a cancelled context surfaces as context.Canceled. Object keys are visited
// in sorted order so the produced access positions are reproducible.
func refEvaluate(ctx context.Context, segs []refSeg, doc interface{}, budget int) ([]refMatch, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ambiguous := false
	for _, seg := range segs {
		if seg.ambiguous() {
			ambiguous = true
		}
	}
	matches := []refMatch{{value: doc}}
	for _, seg := range segs {
		next := []refMatch{}
		for _, match := range matches {
			if seg.kind == refChild {
				nm, err := refSelect(match, seg.key)
				if err != nil {
					if ambiguous {
						continue
					}
					return nil, err
				}
				next = append(next, nm)
				continue
			}
			expanded, err := refApplyAmbiguous(ctx, seg, match, budget)
			if err != nil {
				return nil, err
			}
			next = append(next, expanded...)
		}
		matches = next
	}
	return matches, nil
}

// refSelect implements the plain .name / [n] / ["name"] selector.
func refSelect(match refMatch, key refKey) (refMatch, error) {
	switch o := match.value.(type) {
	case []interface{}:
		if !key.isIndex {
			return refMatch{}, fmt.Errorf("could not select value, invalid key: expected number but got %s (string)", key.name)
		}
		i := key.index
		p := i
		if i < 0 {
			p = len(o) + i
		}
		// An out-of-range index is a nil match, not an error: the production
		// selector reports (nil, key, nil) for plain paths.
		if p < 0 || p >= len(o) {
			return refMatch{keys: appendRefKey(match.keys, key.asObjectKey()), value: nil}, nil
		}
		return refMatch{keys: appendRefKey(match.keys, key.asObjectKey()), value: o[p]}, nil
	case map[string]interface{}:
		k := key.asObjectKey()
		v, ok := o[k]
		if !ok {
			return refMatch{}, fmt.Errorf("unknown key %s", k)
		}
		return refMatch{keys: appendRefKey(match.keys, k), value: v}, nil
	default:
		return refMatch{}, fmt.Errorf("unsupported value type %T for select, expected map[string]interface{}, []interface{} or Array", match.value)
	}
}

func sortedObjectKeys(o map[string]interface{}) []string {
	keys := make([]string, 0, len(o))
	for k := range o {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// refWildcardMatches implements [*] / .* with reproducible object order.
func refWildcardMatches(match refMatch) []refMatch {
	switch o := match.value.(type) {
	case []interface{}:
		out := make([]refMatch, 0, len(o))
		for i, v := range o {
			out = append(out, refMatch{keys: appendRefKey(match.keys, strconv.Itoa(i)), value: v})
		}
		return out
	case map[string]interface{}:
		out := make([]refMatch, 0, len(o))
		for _, k := range sortedObjectKeys(o) {
			out = append(out, refMatch{keys: appendRefKey(match.keys, k), value: o[k]})
		}
		return out
	default:
		return nil
	}
}

// refNegmax clamps negative and oversized slice bounds exactly like the
// production negmax helper.
func refNegmax(n, length int) int {
	if n < 0 {
		n = length + n
		if n < 0 {
			n = 0
		}
	} else if n > length {
		return length
	}
	return n
}

// refSliceMatches implements [min:max:step] on arrays; any other input matches
// nothing, matching the production rangeSelector.
func refSliceMatches(seg refSeg, match refMatch) []refMatch {
	o, ok := match.value.([]interface{})
	if !ok {
		return nil
	}
	min, max, step := 0, math.MaxInt32, 1
	if seg.min != nil {
		min = *seg.min
	}
	if seg.max != nil {
		max = *seg.max
	}
	if seg.step != nil {
		step = *seg.step
	}
	if step == 0 {
		step = 1
	}
	n := len(o)
	min = refNegmax(min, n)
	max = refNegmax(max, n)
	if min > max {
		return nil
	}
	out := []refMatch{}
	if step > 0 {
		for i := min; i < max; i += step {
			out = append(out, refMatch{keys: appendRefKey(match.keys, strconv.Itoa(i)), value: o[i]})
		}
	} else {
		for i := max - 1; i >= min; i += step {
			out = append(out, refMatch{keys: appendRefKey(match.keys, strconv.Itoa(i)), value: o[i]})
		}
	}
	return out
}

// refUnionMatches implements [k1,k2,...]; per-key selection failures are dropped.
func refUnionMatches(seg refSeg, match refMatch) []refMatch {
	out := []refMatch{}
	for _, key := range seg.keys {
		nm, err := refSelect(match, key)
		if err != nil {
			continue
		}
		out = append(out, nm)
	}
	return out
}

// refRecurseMatches implements the .. mapper: the node itself and every descendant
// in pre-order (arrays by index, objects by sorted key), then the sub
// selector is applied to each visited node.
func refRecurseMatches(ctx context.Context, sub refSeg, root refMatch, budget int) ([]refMatch, error) {
	out := []refMatch{}
	visits := 0
	var walk func(node refMatch) error
	walk = func(node refMatch) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		visits++
		if visits > budget {
			return fmt.Errorf("%w after %d recursive-descent visits", errRefBudget, budget)
		}
		switch sub.kind {
		case refChild:
			if nm, err := refSelect(node, sub.key); err == nil {
				out = append(out, nm)
			}
		case refUnion:
			out = append(out, refUnionMatches(sub, node)...)
		case refWildcard:
			out = append(out, refWildcardMatches(node)...)
		}
		switch o := node.value.(type) {
		case []interface{}:
			for i, v := range o {
				if err := walk(refMatch{keys: appendRefKey(node.keys, strconv.Itoa(i)), value: v}); err != nil {
					return err
				}
			}
		case map[string]interface{}:
			for _, k := range sortedObjectKeys(o) {
				if err := walk(refMatch{keys: appendRefKey(node.keys, k), value: o[k]}); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(root); err != nil {
		return nil, err
	}
	return out, nil
}

func refApplyAmbiguous(ctx context.Context, seg refSeg, match refMatch, budget int) ([]refMatch, error) {
	switch seg.kind {
	case refWildcard:
		return refWildcardMatches(match), nil
	case refSlice:
		return refSliceMatches(seg, match), nil
	case refUnion:
		return refUnionMatches(seg, match), nil
	case refRecurse:
		return refRecurseMatches(ctx, *seg.sub, match, budget)
	default:
		return nil, fmt.Errorf("reference internal error: %v is not ambiguous", seg.kind)
	}
}

// refPositionsComparable reports whether the access positions of a parsed
// path are comparable with the repository's GetWithPaths output. When a
// recursive descent is followed by another ambiguous selector, the
// repository reports the collected keys in rotated order (its convertPath
// heuristic only rotates the final segment); values stay fully comparable,
// but access positions for that shape are outside the reference contract,
// mirroring the knownDifferences list of test-suite_test.go.
func refPositionsComparable(segs []refSeg) bool {
	recurseSeen := false
	for _, seg := range segs {
		if seg.kind == refRecurse {
			recurseSeen = true
			continue
		}
		if recurseSeen && seg.ambiguous() {
			return false
		}
	}
	return true
}

func refPathsMap(matches []refMatch) map[string]interface{} {
	m := map[string]interface{}{}
	for _, match := range matches {
		m[match.path()] = match.value
	}
	return m
}

func refValues(matches []refMatch) []interface{} {
	out := make([]interface{}, len(matches))
	for i, match := range matches {
		out[i] = match.value
	}
	return out
}

// ---------------------------------------------------------------------------
// Direct contracts of the restricted reference interpreter
// ---------------------------------------------------------------------------

// TestReferenceGrammar_ExplicitRejection proves that the reference model
// covers only the repository-supported subset and explicitly rejects all
// other syntax. Some rejected constructs are actually accepted by the
// repository (filters, scripts, bracketed floats); the reference model is
// deliberately smaller and must say so instead of silently guessing.
func TestReferenceGrammar_ExplicitRejection(t *testing.T) {
	rejectedByReference := []string{
		`$[?(@.a == 1)]`, // filter expression
		`$.a(@ == 1)`,    // script
		`{#0: $[*]}`,     // placeholder object
		`#0`,             // bare placeholder
		`@.a`,            // current element outside the reference subset
		`$['a']`,         // single quoted char (repository parses it as a rune)
		`$['key']`,       // single quoted multi char keys
		`$[1.5]`,         // non-integer bracketed number
		`$[1:2:3:4]`,     // too many slice components
		`$..[0:2]`,       // recursive descent combined with a range
		`$[1,2:3]`,       // mixed union and slice separators
		`$[]`,            // empty bracket
		`$.`,             // trailing dot
		`$..`,            // recursive descent without target
		``,               // empty expression
		`.a`,             // missing root
	}
	for _, path := range rejectedByReference {
		segs, err := parseReferencePath(path)
		if err == nil {
			t.Errorf("reference grammar rejected contract violated for %q: expected rejection, got %d segments", path, len(segs))
			continue
		}
		if !errors.Is(err, errRefUnsupported) {
			t.Errorf("rejection of %q must wrap errRefUnsupported, got: %v", path, err)
		}
	}

	// The reference subset is a strict subset of what the repository accepts.
	repoAcceptsButReferenceRejects := []string{
		`$[?(@.a == 1)]`,
		`$.a(@ == 1)`,
		`$[1.5]`,
		`$['a']`,
	}
	for _, path := range repoAcceptsButReferenceRejects {
		if _, err := jsonpathNew(path); err != nil {
			t.Errorf("precondition: repository is expected to accept %q, got %v", path, err)
			continue
		}
		if _, err := parseReferencePath(path); !errors.Is(err, errRefUnsupported) {
			t.Errorf("reference must explicitly reject %q, got %v", path, err)
		}
	}

	// Syntax rejected by both layers: rejection must land at parse time.
	bothReject := []string{`$['key']`, `$[1:2:3:4]`, `$..[0:2]`, `$[1,2:3]`}
	for _, path := range bothReject {
		if _, err := jsonpathNew(path); err == nil {
			t.Errorf("precondition: repository is expected to reject %q", path)
		}
		if _, err := parseReferencePath(path); !errors.Is(err, errRefUnsupported) {
			t.Errorf("reference must explicitly reject %q, got %v", path, err)
		}
	}
}

// TestReferenceGrammar_AcceptsSubset checks that the declared subset parses.
func TestReferenceGrammar_AcceptsSubset(t *testing.T) {
	accepted := []string{
		`$`,
		`$.a`,
		`$[0]`,
		`$[-2]`,
		`$["a b"]`,
		`$[*]`,
		`$.*`,
		`$[1, 3]`,
		`$["a", "b"]`,
		`$[0:2]`,
		`$[-2:]`,
		`$[:-1]`,
		`$[::2]`,
		`$[0:3:-1]`,
		`$..x`,
		`$..*`,
		`$..[0]`,
		`$..["x", "y"]`,
		`$.a.b[*][0:3].c..d`,
	}
	for _, path := range accepted {
		segs, err := parseReferencePath(path)
		if err != nil {
			t.Errorf("reference grammar rejected declared subset member %q: %v", path, err)
			continue
		}
		if len(segs) == 0 && path != "$" {
			t.Errorf("parsed %q produced no segments", path)
		}
	}
}

// TestReferenceInterpreter_VisitBudget enforces the resource-budget
// contract of recursive descent: exhaustion reports the causal budget error
// instead of silently truncating results.
func TestReferenceInterpreter_VisitBudget(t *testing.T) {
	doc := mustDecode(t, `{"a":{"b":{"c":[1,2]}}}`)
	segs, err := parseReferencePath(`$..*`)
	if err != nil {
		t.Fatal(err)
	}
	const budget = 3
	matches, err := refEvaluate(context.Background(), segs, doc, budget)
	if !errors.Is(err, errRefBudget) {
		t.Fatalf("first contract violation: budget=%d, want errRefBudget, got matches=%v err=%v", budget, matches, err)
	}
	if matches != nil {
		t.Errorf("budget exhaustion must not return partial matches, got %v", matches)
	}
	// A generous budget on the same document succeeds: budget exhaustion is a
	// causal, recoverable condition, not a persistent state change.
	matches, err = refEvaluate(context.Background(), segs, doc, refDefaultBudget)
	if err != nil {
		t.Fatalf("same input after budget exhaustion must recover, got %v", err)
	}
	if len(matches) != 5 {
		t.Errorf("first value contract violation after recovery: want 5 matches, got %d: %v", len(matches), matches)
	}
}

// TestReferenceInterpreter_Cancellation preserves the caller's cancellation
// cause instead of masking it behind a different error.
func TestReferenceInterpreter_Cancellation(t *testing.T) {
	doc := mustDecode(t, `{"a":{"b":[1,2,3]}}`)
	segs, err := parseReferencePath(`$..*`)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := refEvaluate(ctx, segs, doc, refDefaultBudget); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation contract violated: want context.Canceled, got %v", err)
	}
}

// TestReferenceInterpreter_DeterministicOrder proves object traversal is
// reproducible: repeated evaluations produce exactly equal ordered access
// positions and values.
func TestReferenceInterpreter_DeterministicOrder(t *testing.T) {
	doc := mustDecode(t, `{"z":{"y":1,"a":2},"m":[{"x":3},{"a":4}],"a":5}`)
	segs, err := parseReferencePath(`$..*`)
	if err != nil {
		t.Fatal(err)
	}
	first, err := refEvaluate(context.Background(), segs, doc, refDefaultBudget)
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 10; attempt++ {
		again, err := refEvaluate(context.Background(), segs, doc, refDefaultBudget)
		if err != nil {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
		if diff := firstDiffRefMatchSlice(first, again); diff != "" {
			t.Fatalf("recursive descent order is not reproducible at attempt %d:\n%s", attempt, diff)
		}
	}
}

func firstDiffRefMatchSlice(want, got []refMatch) string {
	for i := 0; i < len(want) || i < len(got); i++ {
		if i >= len(want) || i >= len(got) || want[i].path() != got[i].path() || canonicalJSON(want[i].value) != canonicalJSON(got[i].value) {
			return fmt.Sprintf("first violation at match index %d:\n  want: %s = %s\n  got:  %s = %s",
				i,
				pathOrIndex(i, want), canonicalOrNil(i, want),
				pathOrIndex(i, got), canonicalOrNil(i, got))
		}
	}
	return ""
}

func pathOrIndex(i int, matches []refMatch) string {
	if i < len(matches) {
		return matches[i].path()
	}
	return "<missing>"
}

func canonicalOrNil(i int, matches []refMatch) string {
	if i >= len(matches) {
		return "<missing>"
	}
	return canonicalJSON(matches[i].value)
}
