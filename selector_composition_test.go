// Acceptance story for "jsonpath: selector composition, placeholder binding
// and failure isolation".
//
// Every case enters through the repository's public entry points
// (jsonpath.New / jsonpath.Get / jsonpath.GetWithPaths /
// jsonpath.Language().NewEvaluableWithContext /
// jsonpath.PlaceholderExtension().NewEvaluable). Parser results are
// cross-checked against the independent restricted reference interpreter in
// reference_test.go, comparing both values and access positions. No internal
// final objects are fabricated.
package jsonpath_test

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/PaesslerAG/jsonpath"
)

// ---------------------------------------------------------------------------
// shared helpers
// ---------------------------------------------------------------------------

// jsonpathNew is the public parse entry used by the whole acceptance story.
func jsonpathNew(path string) (interface{}, error) {
	return jsonpath.New(path)
}

// decodeDocument is the only way fixtures become documents; corrupted input
// keeps its causal error.
func decodeDocument(data string) (interface{}, error) {
	var v interface{}
	if err := json.Unmarshal([]byte(data), &v); err != nil {
		return nil, fmt.Errorf("corrupted input document: %w", err)
	}
	return v, nil
}

func mustDecode(t *testing.T, data string) interface{} {
	t.Helper()
	doc, err := decodeDocument(data)
	if err != nil {
		t.Fatalf("fixture is not valid JSON: %v", err)
	}
	return doc
}

// canonicalJSON renders a value deterministically (encoding/json sorts map
// keys), so results can be compared independent of Go map iteration order.
func canonicalJSON(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("cannot canonicalize %T: %v", v, err))
	}
	return string(b)
}

// canonicalMultiset renders a value list order-insensitively.
func canonicalMultiset(values []interface{}) string {
	parts := make([]string, len(values))
	for i, v := range values {
		parts[i] = canonicalJSON(v)
	}
	sort.Strings(parts)
	return strings.Join(parts, "\n")
}

// repoOutcome records everything a caller can observe plus the internal
// invariants that prove JSONPath semantics: the value (or its error) and the
// access positions with their values (or their error).
type repoOutcome struct {
	value    interface{}
	valueErr error
	paths    map[string]interface{}
	pathsErr error
}

func (o repoOutcome) canonical() string {
	value := "value=" + canonicalJSON(o.value)
	if o.valueErr != nil {
		value = "valueErr=" + o.valueErr.Error()
	}
	paths := "paths=" + canonicalJSON(o.paths)
	if o.pathsErr != nil {
		paths = "pathsErr=" + o.pathsErr.Error()
	}
	return value + "\n" + paths
}

// runRepo evaluates one path against one document through the public entry
// points only: once as a plain evaluation and once with full-path collection.
func runRepo(t *testing.T, path string, doc interface{}) repoOutcome {
	t.Helper()
	eval, err := jsonpath.New(path)
	if err != nil {
		t.Fatalf("public entry jsonpath.New(%q) unexpectedly failed: %v", path, err)
	}
	value, valueErr := eval(context.Background(), doc)

	ctx := context.WithValue(context.Background(), jsonpath.CollectFullPathsContextKey{}, true)
	pathEval, err := jsonpath.Language().NewEvaluableWithContext(ctx, path)
	if err != nil {
		t.Fatalf("public entry NewEvaluableWithContext(%q) unexpectedly failed: %v", path, err)
	}
	rawPaths, pathsErr := pathEval(ctx, doc)
	paths, _ := rawPaths.(map[string]interface{})
	return repoOutcome{value: value, valueErr: valueErr, paths: paths, pathsErr: pathsErr}
}

// assertMatchesReference feeds the same path to the independent restricted
// reference interpreter and compares values and access positions with what the
// production evaluator reported.
func assertMatchesReference(t *testing.T, caseName, path string, doc interface{}, got repoOutcome) {
	t.Helper()
	segs, err := parseReferencePath(path)
	if err != nil {
		t.Fatalf("[%s] reference grammar rejected path %q inside the declared subset: %v", caseName, path, err)
	}
	ambiguous := false
	for _, seg := range segs {
		if seg.ambiguous() {
			ambiguous = true
		}
	}
	ref, refErr := refEvaluate(context.Background(), segs, doc, refDefaultBudget)

	if !ambiguous {
		// Plain path: exactly one result, errors are part of the contract.
		if (got.valueErr != nil) != (refErr != nil) {
			t.Fatalf("[%s] first contract violation for %s: error presence mismatch\n  repo valueErr: %v\n  reference err: %v",
				caseName, path, got.valueErr, refErr)
		}
		if refErr != nil {
			if got.valueErr.Error() != refErr.Error() {
				t.Fatalf("[%s] first contract violation for %s: error text mismatch\n  repo:      %v\n  reference: %v",
					caseName, path, got.valueErr, refErr)
			}
			return
		}
		if len(ref) != 1 {
			t.Fatalf("[%s] reference produced %d matches for plain path %s", caseName, len(ref), path)
		}
		if !reflect.DeepEqual(got.value, ref[0].value) {
			t.Fatalf("[%s] first contract violation for %s: value mismatch\n  repo:      %s\n  reference: %s",
				caseName, path, canonicalJSON(got.value), canonicalJSON(ref[0].value))
		}
		if got.pathsErr != nil {
			t.Fatalf("[%s] plain path %s reported value but path collection failed: %v", caseName, path, got.pathsErr)
		}
		wantPaths := refPathsMap(ref)
		if !reflect.DeepEqual(got.paths, wantPaths) {
			t.Fatalf("[%s] first contract violation for %s: access position mismatch\n  repo:      %s\n  reference: %s",
				caseName, path, canonicalJSON(got.paths), canonicalJSON(wantPaths))
		}
		return
	}

	// Ambiguous path: the production evaluator never reports an error and
	// collects matches; order over objects is not part of the contract, so
	// values compare as multisets while access positions compare exactly.
	if got.valueErr != nil {
		t.Fatalf("[%s] ambiguous path %s unexpectedly failed: %v", caseName, path, got.valueErr)
	}
	if refErr != nil {
		t.Fatalf("[%s] reference failed for %s: %v", caseName, path, refErr)
	}
	gotValues, ok := got.value.([]interface{})
	if !ok {
		t.Fatalf("[%s] ambiguous path %s must return []interface{}, got %T", caseName, path, got.value)
	}
	if diff := firstDiffMultiset(canonicalMultiset(gotValues), canonicalMultiset(refValues(ref))); diff != "" {
		t.Fatalf("[%s] first contract violation for %s: value multiset mismatch\n%s\n  repo:      %v\n  reference: %v",
			caseName, path, diff, got.value, refValues(ref))
	}
	if got.pathsErr != nil {
		t.Fatalf("[%s] ambiguous path %s path collection failed: %v", caseName, path, got.pathsErr)
	}
	if !refPositionsComparable(segs) {
		// Recursive descent followed by another ambiguous selector: the
		// repository reports rotated access positions (documented
		// deviation); values above remain fully checked.
		return
	}
	wantPaths := refPathsMap(ref)
	if diff := firstDiffPathsMap(wantPaths, got.paths); diff != "" {
		t.Fatalf("[%s] first contract violation for %s: access position mismatch\n%s", caseName, path, diff)
	}
}

func firstDiffMultiset(want, got string) string {
	if want == got {
		return ""
	}
	return fmt.Sprintf("  want multiset: %q\n  got multiset:  %q", want, got)
}

// firstDiffPathsMap reports the first (sorted) access position at which two
// path maps disagree, keeping the violating position and both values.
func firstDiffPathsMap(want, got map[string]interface{}) string {
	keys := map[string]bool{}
	for k := range want {
		keys[k] = true
	}
	for k := range got {
		keys[k] = true
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	for _, k := range sorted {
		w, wok := want[k]
		g, gok := got[k]
		if !wok || !gok || !reflect.DeepEqual(w, g) {
			return fmt.Sprintf("  position %s:\n    want: %s (present=%v)\n    got:  %s (present=%v)",
				k, canonicalJSON(w), wok, canonicalJSON(g), gok)
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Fixed selector-level and full-path-level combination cases.
//
// Every fixed case states the caller-visible result (want / wantErr) and the
// internal access positions (wantPaths). The same parser result is then
// handed to the independent restricted reference interpreter and compared
// again on value and access positions.
// ---------------------------------------------------------------------------

type compositionCase struct {
	name      string
	path      string
	data      string
	want      interface{}
	wantPaths map[string]interface{}
	wantErr   string
}

func compareMultisetWant(want, got []interface{}) string {
	w := strings.Split(canonicalMultiset(want), "\n")
	g := strings.Split(canonicalMultiset(got), "\n")
	if len(w) == 1 && w[0] == "" {
		w = nil
	}
	if len(g) == 1 && g[0] == "" {
		g = nil
	}
	for i := 0; i < len(w) || i < len(g); i++ {
		var ws, gs string
		if i < len(w) {
			ws = w[i]
		} else {
			ws = "<missing>"
		}
		if i < len(g) {
			gs = g[i]
		} else {
			gs = "<missing>"
		}
		if ws != gs {
			return fmt.Sprintf("first violation at sorted match index %d:\n  want: %s\n  got:  %s", i, ws, gs)
		}
	}
	return ""
}

func (tc compositionCase) check(t *testing.T) {
	t.Helper()
	doc := mustDecode(t, tc.data)
	got := runRepo(t, tc.path, doc)
	segs, err := parseReferencePath(tc.path)
	if err != nil {
		t.Fatalf("[%s] fixture path is outside the reference subset: %v", tc.name, err)
	}
	ambiguous := false
	for _, seg := range segs {
		if seg.ambiguous() {
			ambiguous = true
		}
	}

	if tc.wantErr != "" {
		if got.valueErr == nil {
			t.Fatalf("[%s] expected error %q but got value %v", tc.name, tc.wantErr, got.value)
		}
		if got.valueErr.Error() != tc.wantErr {
			t.Errorf("[%s] first error contract violation for %s\n  want: %s\n  got:  %s",
				tc.name, tc.path, tc.wantErr, got.valueErr.Error())
		}
		if (got.pathsErr == nil) != (got.valueErr == nil) {
			t.Errorf("[%s] plain path error must keep its causality through path collection, pathsErr=%v", tc.name, got.pathsErr)
		}
		// The reference model must predict the identical error.
		assertMatchesReference(t, tc.name, tc.path, doc, got)
		return
	}

	if got.valueErr != nil {
		t.Fatalf("[%s] unexpected evaluation error for %s: %v", tc.name, tc.path, got.valueErr)
	}

	if ambiguous {
		gotValues, ok := got.value.([]interface{})
		if !ok {
			t.Fatalf("[%s] ambiguous path %s must return []interface{}, got %T", tc.name, tc.path, got.value)
		}
		wantValues, ok := tc.want.([]interface{})
		if !ok {
			t.Fatalf("[%s] fixture bug: want must be []interface{} for ambiguous path", tc.name)
		}
		if diff := compareMultisetWant(wantValues, gotValues); diff != "" {
			t.Errorf("[%s] caller-visible result mismatch for %s\n%s\n  want full: %v\n  got full:  %v",
				tc.name, tc.path, diff, wantValues, gotValues)
		}
	} else if !reflect.DeepEqual(got.value, tc.want) {
		t.Errorf("[%s] caller-visible result mismatch for %s\n  want: %s\n  got:  %s",
			tc.name, tc.path, canonicalJSON(tc.want), canonicalJSON(got.value))
	}

	if tc.wantPaths != nil {
		if !refPositionsComparable(segs) {
			t.Fatalf("[%s] fixture bug: wantPaths set for a path shape whose access positions the repository reports in rotated order", tc.name)
		}
		if diff := firstDiffPathsMap(tc.wantPaths, got.paths); diff != "" {
			t.Errorf("[%s] access position mismatch for %s\n%s", tc.name, tc.path, diff)
		}
	}

	assertMatchesReference(t, tc.name, tc.path, doc, got)
}

// TestSelectorComposition_SelectorAndFullPathLevels combines selector-level
// cases (one selector after the root) with full-path-level cases (composed
// chains) and anchors both observable values and internal access positions.
func TestSelectorComposition_SelectorAndFullPathLevels(t *testing.T) {
	for _, tc := range compositionCases {
		t.Run(tc.name, tc.check)
	}
}

const unsupportedTypeMsg = "unsupported value type %s for select, expected map[string]interface{}, []interface{} or Array"

var compositionCases = []compositionCase{
	// ----- selector level ------------------------------------------------
	{"selector/child-key", `$.a`, `{"a":1,"b":2}`, 1., obj{`$["a"]`: 1.}, ""},
	{"selector/child-index", `$[1]`, `[10,20,30]`, 20., obj{`$["1"]`: 20.}, ""},
	{"selector/negative-index", `$[-1]`, `[10,20,30]`, 30., obj{`$["-1"]`: 30.}, ""},
	{"selector/negative-index-to-front", `$[-3]`, `[10,20,30]`, 10., obj{`$["-3"]`: 10.}, ""},
	{"selector/index-out-of-bounds", `$[3]`, `[10,20,30]`, nil, obj{`$["3"]`: nil}, ""},
	{"selector/negative-index-out-of-bounds", `$[-4]`, `[10,20,30]`, nil, obj{`$["-4"]`: nil}, ""},
	{"selector/index-on-object-key", `$[1]`, `{"1":"one"}`, "one", obj{`$["1"]`: "one"}, ""},
	{"selector/quoted-key", `$["a"]`, `{"a":1,"b":2}`, 1., obj{`$["a"]`: 1.}, ""},
	{"selector/quoted-special-key", `$["x y"]`, `{"x y":7}`, 7., obj{`$["x y"]`: 7.}, ""},
	{"selector/wildcard-array", `$[*]`, `[10,20]`, arr{10., 20.}, obj{`$["0"]`: 10., `$["1"]`: 20.}, ""},
	{"selector/wildcard-object", `$.*`, `{"b":2,"a":1}`, arr{1., 2.}, obj{`$["a"]`: 1., `$["b"]`: 2.}, ""},
	{"selector/wildcard-empty-array", `$[*]`, `[]`, arr{}, obj{}, ""},
	{"selector/wildcard-on-scalar", `$[*]`, `5`, arr{}, obj{}, ""},
	{"selector/wildcard-empty-object", `$.*`, `{}`, arr{}, obj{}, ""},
	{"selector/slice", `$[1:3]`, `[10,20,30,40]`, arr{20., 30.}, obj{`$["1"]`: 20., `$["2"]`: 30.}, ""},
	{"selector/slice-stepped", `$[0:4:2]`, `[10,20,30,40]`, arr{10., 30.}, obj{`$["0"]`: 10., `$["2"]`: 30.}, ""},
	{"selector/slice-on-object", `$[0:2]`, `{"a":1}`, arr{}, obj{}, ""},
	{"selector/union", `$[0,2]`, `[10,20,30]`, arr{10., 30.}, obj{`$["0"]`: 10., `$["2"]`: 30.}, ""},
	{"selector/union-quoted", `$["a","c"]`, `{"a":1,"b":2,"c":3}`, arr{1., 3.}, obj{`$["a"]`: 1., `$["c"]`: 3.}, ""},
	{"selector/union-mixed-on-object", `$[0,"a"]`, `{"0":"zero","a":"A"}`, arr{"zero", "A"}, obj{`$["0"]`: "zero", `$["a"]`: "A"}, ""},
	{"selector/union-mixed-on-array-drops-string", `$[0,"a"]`, `[10,20]`, arr{10.}, obj{`$["0"]`: 10.}, ""},
	{"selector/union-duplicate", `$[1,1]`, `[10,20]`, arr{20., 20.}, obj{`$["1"]`: 20.}, ""},
	{"selector/recurse-key", `$..x`, `{"x":1,"n":{"x":2}}`, arr{1., 2.}, obj{`$["x"]`: 1., `$["n"]["x"]`: 2.}, ""},
	{"selector/recurse-wildcard", `$..*`, `{"a":[1]}`, arr{arr{1.}, 1.}, obj{`$["a"]`: arr{1.}, `$["a"]["0"]`: 1.}, ""},
	{"selector/recurse-index", `$..[0]`, `{"list":[[7],[8,9]]}`, arr{arr{7.}, 7., 8.},
		obj{
			`$["list"]["0"]`:       arr{7.},
			`$["list"]["0"]["0"]`: 7.,
			`$["list"]["1"]["0"]`: 8.,
		}, ""},
	{"selector/recurse-quoted-union", `$..["x","a"]`, `{"a":{"x":1},"x":4}`,
		arr{1., 4., obj{"x": 1.}},
		obj{
			`$["x"]`:           4.,
			`$["a"]`:           obj{"x": 1.},
			`$["a"]["x"]`:      1.,
		}, ""},
	{"selector/recurse-on-scalar", `$..x`, `5`, arr{}, obj{}, ""},

	// ----- full path level -----------------------------------------------
	{"path/nested-plain", `$.a.b[1]`, `{"a":{"b":[10,20]}}`, 20., obj{`$["a"]["b"]["1"]`: 20.}, ""},
	{"path/wildcard-then-key-drops-missing", `$.items[*].id`,
		`{"items":[{"id":1},{"id":2},{"x":0}]}`,
		arr{1., 2.},
		obj{`$["items"]["0"]["id"]`: 1., `$["items"]["1"]["id"]`: 2.}, ""},
	{"path/slice-then-key", `$[1:3].v`,
		`[{"v":"a"},{"v":"b"},{"v":"c"},{"v":"d"}]`,
		arr{"b", "c"},
		obj{`$["1"]["v"]`: "b", `$["2"]["v"]`: "c"}, ""},
	{"path/union-then-wildcard", `$[0,1].*`,
		`[{"a":1},{"b":2}]`,
		arr{1., 2.},
		obj{`$["0"]["a"]`: 1., `$["1"]["b"]`: 2.}, ""},
	{"path/wildcard-then-recurse", `$[*]..y`,
		`[{"y":1},{"q":{"y":3}}]`,
		arr{1., 3.},
		obj{`$["0"]["y"]`: 1., `$["1"]["q"]["y"]`: 3.}, ""},
	// recursive descent followed by an ambiguous selector: values are
	// contractual, access positions are reported rotated by the repository
	// (documented deviation, see refPositionsComparable).
	{"path/recurse-then-slice", `$..v[0:2]`,
		`{"a":{"v":[1,2,3]},"b":{"v":[9]}}`,
		arr{1., 2., 9.},
		nil, ""},
	{"path/wildcard-wildcard", `$[*][*]`,
		`[[1,2],[],[3]]`,
		arr{1., 2., 3.},
		obj{`$["0"]["0"]`: 1., `$["0"]["1"]`: 2., `$["2"]["0"]`: 3.}, ""},
	{"path/wildcard-wildcard-empty-inner", `$[*][*]`,
		`[[],[[]],[]]`,
		arr{arr{}},
		obj{`$["1"]["0"]`: arr{}}, ""},
	{"path/prefix-missing-key-isolated", `$.a[*]`,
		`{"b":[1]}`,
		arr{}, obj{}, ""},
	{"path/list-of-named-items", `$.list[*].name`,
		`{"list":[{"name":"x"},{"name":"y"}]}`,
		arr{"x", "y"},
		obj{`$["list"]["0"]["name"]`: "x", `$["list"]["1"]["name"]`: "y"}, ""},

	// ----- deep empty collections ----------------------------------------
	{"deep-empty/wildcard-chain", `$.a[*].b[*]`,
		`{"a":[{"b":[]},{"b":[]}]}`,
		arr{}, obj{}, ""},
	{"deep-empty/slice-over-keyless-objects", `$[0:5].x`,
		`[{},{},{}]`,
		arr{}, obj{}, ""},
	{"deep-empty/recurse-through-empty-map-and-array", `$..[*]`,
		`{"a":{"b":{"c":[]}}}`,
		arr{obj{"b": obj{"c": arr{}}}, obj{"c": arr{}}, arr{}},
		obj{
			`$["a"]`:     obj{"b": obj{"c": arr{}}},
			`$["a"]["b"]`: obj{"c": arr{}},
			`$["a"]["b"]["c"]`: arr{},
		}, ""},
	{"deep-empty/nested-empty-recurse-key", `$..missing`,
		`{"a":{},"b":[{},[[]]]}`,
		arr{}, obj{}, ""},

	// ----- type mismatch (plain paths keep the causal error) -------------
	{"type-mismatch/index-on-string", `$[0]`, `"text"`, nil, nil,
		fmt.Sprintf(unsupportedTypeMsg, "string")},
	{"type-mismatch/key-on-array", `$.k`, `[1]`, nil, nil,
		"could not select value, invalid key: expected number but got k (string)"},
	{"type-mismatch/key-on-nil", `$.a`, `null`, nil, nil,
		fmt.Sprintf(unsupportedTypeMsg, "<nil>")},
	{"type-mismatch/key-on-number", `$.a`, `5`, nil, nil,
		fmt.Sprintf(unsupportedTypeMsg, "float64")},
	{"type-mismatch/key-on-bool", `$.a`, `true`, nil, nil,
		fmt.Sprintf(unsupportedTypeMsg, "bool")},
	{"type-mismatch/chained-key-on-scalar", `$.a.b`, `{"a":"str"}`, nil, nil,
		fmt.Sprintf(unsupportedTypeMsg, "string")},
	{"type-mismatch/unknown-object-key", `$.missing`, `{"a":1}`, nil, nil,
		"unknown key missing"},
	{"type-mismatch/index-missing-on-object", `$[2]`, `{"1":1}`, nil, nil,
		"unknown key 2"},
}
