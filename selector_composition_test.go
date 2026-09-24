package jsonpath_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"testing"

	"github.com/PaesslerAG/gval"
	"github.com/PaesslerAG/jsonpath"
)

type compositionCase struct {
	name    string
	path    string
	data    string
	wantErr string
}

// TestSelectorCompositionMatchesReferenceModel drives the repository parser
// and evaluator through their public entry points (jsonpath.New / Get /
// GetWithPaths) for selector level and full path level compositions and
// cross-checks both the returned values and the recorded access positions
// against the independent restricted reference interpreter.
func TestSelectorCompositionMatchesReferenceModel(t *testing.T) {
	cases := []compositionCase{
		// selector level: negative indices and slices
		{"selector/negative-index-last", `$[-1]`, `["a","b","c"]`, ""},
		{"selector/negative-index-first", `$[-3]`, `["a","b","c"]`, ""},
		{"selector/negative-index-out-of-range", `$[-3]`, `["a","b"]`, ""},
		{"selector/index-out-of-range", `$[5]`, `["a","b"]`, ""},
		{"selector/slice-basic", `$[1:3]`, `["a","b","c","d"]`, ""},
		{"selector/slice-negative-end", `$[:-1]`, `["a","b","c","d"]`, ""},
		{"selector/slice-negative-start", `$[-2:]`, `["a","b","c","d"]`, ""},
		{"selector/slice-step", `$[0:4:2]`, `["a","b","c","d"]`, ""},
		{"selector/slice-reverse", `$[::-1]`, `["a","b","c","d"]`, ""},
		{"selector/slice-empty-interval", `$[2:2]`, `["a","b","c","d"]`, ""},
		{"selector/slice-on-object", `$[0:2]`, `{"a":1}`, ""},

		// selector level: object/array wildcards and unions
		{"selector/wildcard-object-sorted-order", `$.*`, `{"b":1,"a":2,"c":3}`, ""},
		{"selector/wildcard-array", `$[*]`, `[1,"x",null]`, ""},
		{"selector/wildcard-on-scalar", `$.*`, `"text"`, ""},
		{"selector/union-array-negative", `$[0,-1]`, `["a","b","c"]`, ""},
		{"selector/union-object", `$["a","c"]`, `{"a":1,"b":2,"c":3}`, ""},
		{"selector/union-out-of-range-keeps-nil", `$[0,9]`, `["a","b"]`, ""},
		{"selector/union-on-scalar", `$[0,"a"]`, `5`, ""},

		// selector level: recursive descent
		{"selector/recursive-key", `$..x`, `{"a":{"x":1},"b":[{"x":2}],"x":4}`, ""},
		{"selector/recursive-wildcard", `$..*`, `{"a":{"x":1},"b":[2]}`, ""},
		{"selector/recursive-union", `$..["x","y"]`, `{"a":{"x":1,"y":2},"y":3}`, ""},

		// full path level: combinations of the selectors above
		{"path/root", `$`, `{"a":[1]}`, ""},
		{"path/chained-negative-index-then-key", `$.a[-1].b`, `{"a":[{"b":1},{"b":2}]}`, ""},
		{"path/key-then-slice", `$.a[1:3]`, `{"a":[1,2,3,4]}`, ""},
		{"path/slice-then-key", `$.a[0:2].b`, `{"a":[{"b":1},{"b":2},{"b":3}]}`, ""},
		{"path/wildcard-then-key-sorted-order", `$.a.*.b`, `{"a":{"m":{"b":1},"n":{"b":2}}}`, ""},
		{"path/recursive-then-index", `$..x[0]`, `{"x":[9],"a":{"x":[7]}}`, ""},
		{"path/missing-node-under-ambiguous-selector", `$.a.b[*]`, `{"a":{}}`, ""},
		{"path/scalar-under-ambiguous-selector", `$.a.b[*]`, `{"a":{"b":5}}`, ""},
		{"path/deep-recursive-empty", `$.a.b..*`, `{"a":{"b":{}}}`, ""},

		// evaluation errors that the reference model agrees with
		{"error/unknown-key-on-plain-path", `$.missing`, `{"a":1}`, "unknown key missing"},
		{"error/type-mismatch-index-on-string", `$[0]`, `"text"`, "unsupported value type string for select, expected map[string]interface{}, []interface{} or Array"},
		{"error/type-mismatch-key-on-array", `$.a`, `[1,2]`, "could not select value, invalid key: expected number but got a (string)"},
		{"error/type-mismatch-deep", `$.a.b`, `{"a":1}`, "unsupported value type float64 for select, expected map[string]interface{}, []interface{} or Array"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			steps, err := parseRefPath(tc.path)
			if err != nil {
				t.Fatalf("first violation at reference parse of %q: %v", tc.path, err)
			}
			doc := mustJSON(t, tc.data)
			wantMatches, refErr := evalRefPath(steps, doc)

			got, err := jsonpath.Get(tc.path, doc)
			if tc.wantErr != "" {
				if err == nil || err.Error() != tc.wantErr {
					t.Fatalf("first violation evaluating %q on %s:\n got error: %v\nwant error: %s", tc.path, tc.data, err, tc.wantErr)
				}
				if refErr == nil || refErr.Error() != tc.wantErr {
					t.Fatalf("reference model disagrees on %q error: got %v want %s", tc.path, refErr, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("first violation evaluating %q on %s: %v", tc.path, tc.data, err)
			}
			if refErr != nil {
				t.Fatalf("reference model failed on %q: %v", tc.path, refErr)
			}

			if want := refExpectValue(steps, wantMatches); !reflect.DeepEqual(got, want) {
				t.Errorf("value mismatch for %s on %s:\n got: %#v\nwant: %#v", tc.path, tc.data, got, want)
			}

			gotPaths, err := jsonpath.GetWithPaths(tc.path, doc)
			if err != nil {
				t.Fatalf("GetWithPaths(%s) failed: %v", tc.path, err)
			}
			if want := refExpectPaths(wantMatches); !reflect.DeepEqual(gotPaths, want) {
				t.Errorf("access position mismatch for %s on %s:\n got: %#v\nwant: %#v", tc.path, tc.data, gotPaths, want)
			}
		})
	}
}

// TestIndexEndpointOffByOneMutations fixes inputs that kill common endpoint
// mutations: inclusive slice ends and a shifted negative index base. The
// "mutant" column is what such an implementation would return; the
// repository must return "want" and must never return the mutant result.
func TestIndexEndpointOffByOneMutations(t *testing.T) {
	doc := mustJSON(t, `["a","b","c","d"]`)
	cases := []struct {
		name   string
		path   string
		want   interface{}
		mutant interface{}
	}{
		{"slice-end-is-exclusive", `$[1:3]`, arr{"b", "c"}, arr{"b", "c", "d"}},
		{"slice-single-element", `$[0:1]`, arr{"a"}, arr{"a", "b"}},
		{"slice-identical-endpoints", `$[2:2]`, arr{}, arr{"c"}},
		{"slice-negative-end-exclusive", `$[:-1]`, arr{"a", "b", "c"}, arr{"a", "b", "c", "d"}},
		{"slice-negative-start", `$[-2:]`, arr{"c", "d"}, arr{"b", "c", "d"}},
		{"negative-index-last", `$[-1]`, "d", "c"},
		{"negative-index-first", `$[-4]`, "a", nil},
		{"slice-reverse-full", `$[::-1]`, arr{"d", "c", "b", "a"}, arr{"c", "b", "a"}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got, err := jsonpath.Get(tc.path, doc)
			if err != nil {
				t.Fatalf("first violation evaluating %s: %v", tc.path, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("endpoint mismatch for %s:\n got: %#v\nwant: %#v", tc.path, got, tc.want)
			}
			if reflect.DeepEqual(got, tc.mutant) {
				t.Errorf("case %s does not catch the off-by-one mutant: got %#v", tc.path, got)
			}
			// the independent reference model must make the same call
			steps, err := parseRefPath(tc.path)
			if err != nil {
				t.Fatalf("reference model rejected %s: %v", tc.path, err)
			}
			matches, err := evalRefPath(steps, doc)
			if err != nil {
				t.Fatalf("reference model failed %s: %v", tc.path, err)
			}
			if want := refExpectValue(steps, matches); !reflect.DeepEqual(want, tc.want) {
				t.Errorf("reference model disagrees on %s: got %#v want %#v", tc.path, want, tc.want)
			}
		})
	}
}

// TestReferenceModelRejectsUnsupportedSyntax pins down the boundary of the
// restricted reference model: it explicitly rejects syntax outside the
// repository subset it models, including inputs the repository itself
// accepts through other parts of its public API.
func TestReferenceModelRejectsUnsupportedSyntax(t *testing.T) {
	rejectedByReference := []string{
		`$[?(@.a==1)]`, // filter expression
		`$.a(@==1)`,    // script expression
		`{#0: $[*]}`,   // placeholder object
		`@.a`,          // current element
		`$..[0:2]`,     // recursive descent cannot be combined with a slice
		`$.a['b']`,     // single quoted keys are not supported
		`$[1:2:3:4]`,   // too many slice parameters
		`$.a.`,         // trailing dot
	}
	for _, path := range rejectedByReference {
		if _, err := parseRefPath(path); err == nil {
			t.Errorf("reference model must reject %q", path)
		}
	}

	// inputs outside the repository grammar are rejected by both layers
	rejectedByRepository := []string{`$..[0:2]`, `$.a['b']`, `$[1:2:3:4]`, `$.a.`}
	for _, path := range rejectedByRepository {
		if _, err := jsonpath.New(path); err == nil {
			t.Errorf("repository parser must reject %q", path)
		}
	}

	// inputs the repository accepts but the restricted reference does not model
	acceptedByRepositoryOnly := []string{`$[?(@.a==1)]`, `$.a(@==1)`, `@.a`}
	for _, path := range acceptedByRepositoryOnly {
		if _, err := jsonpath.New(path); err != nil {
			t.Errorf("repository parser should accept %q: %v", path, err)
		}
	}
	if _, err := jsonpath.PlaceholderExtension().NewEvaluable(`{#0: $[*]}`); err != nil {
		t.Errorf("placeholder extension should accept {#0: $[*]}: %v", err)
	}
}
