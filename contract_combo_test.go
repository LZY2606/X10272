package jsonpath_test

// Regression material for the contract:
// "jsonpath: selector composition, placeholder binding and failure isolation".
//
// Every case enters through the public API (jsonpath.New / jsonpath.Get /
// jsonpath.GetWithPaths / jsonpath.Language / jsonpath.PlaceholderExtension)
// and is cross-checked against refPath, the independent restricted reference
// interpreter in reference_interpreter_test.go. Failure reports carry the
// first violating visit location plus the path, document and error layer so
// a regression is never reduced to a bare true/false.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/PaesslerAG/gval"
	"github.com/PaesslerAG/jsonpath"
	"github.com/google/go-cmp/cmp"
)

func mustJSONDoc(t *testing.T, doc string) interface{} {
	t.Helper()
	var v interface{}
	if err := json.Unmarshal([]byte(doc), &v); err != nil {
		t.Fatalf("invalid fixture JSON: %v", err)
	}
	return v
}

func jsonSnapshot(v interface{}) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// evalWithPaths drives the public CollectFullPaths diagnostic with a custom
// context (so budget/cancel behavior is observable) and never touches
// package internals.
func evalWithPaths(ctx context.Context, path string, doc interface{}) (interface{}, error) {
	pathCtx := context.WithValue(ctx, jsonpath.CollectFullPathsContextKey{}, true)
	eval, err := jsonpath.Language().NewEvaluableWithContext(pathCtx, path)
	if err != nil {
		return nil, err
	}
	return eval(pathCtx, doc)
}

func evalLangWithPaths(ctx context.Context, lang gval.Language, path string, doc interface{}) (interface{}, error) {
	pathCtx := context.WithValue(ctx, jsonpath.CollectFullPathsContextKey{}, true)
	eval, err := lang.NewEvaluableWithContext(pathCtx, path)
	if err != nil {
		return nil, err
	}
	return eval(pathCtx, doc)
}

// firstLocationViolation walks the sorted union of visit locations and
// returns a description of the first location at which the two result maps
// disagree (missing, extra or different value).
func firstLocationViolation(want, got map[string]interface{}) string {
	all := map[string]struct{}{}
	for k := range want {
		all[k] = struct{}{}
	}
	for k := range got {
		all[k] = struct{}{}
	}
	keys := make([]string, 0, len(all))
	for k := range all {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		w, wok := want[k]
		g, gok := got[k]
		if !wok || !gok || !reflect.DeepEqual(w, g) {
			return fmt.Sprintf("first violating location %q: reference=%#v(present=%v) production=%#v(present=%v)",
				k, w, wok, g, gok)
		}
	}
	return ""
}

func errorLayer(err error) string {
	if err == nil {
		return "nil"
	}
	switch {
	case errors.Is(err, jsonpath.ErrVisitBudgetExceeded):
		return "fatal:visit-budget"
	case errors.Is(err, context.Canceled):
		return "fatal:canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "fatal:deadline"
	default:
		var pe *refParseError
		if errors.As(err, &pe) {
			return "parse"
		}
		return "evaluation"
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func mustNew(t *testing.T, path string) gval.Evaluable {
	t.Helper()
	eval, err := jsonpath.New(path)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return eval
}

// buildDeepNested builds {"a":{"a":...:"leaf"...}} with depth "a" keys.
func buildDeepNested(depth int, key string) map[string]interface{} {
	root := map[string]interface{}{}
	cur := root
	for i := 0; i < depth; i++ {
		next := map[string]interface{}{}
		cur[key] = next
		cur = next
	}
	cur[key] = "leaf"
	return root
}

// assertAgreement runs one full-path query against both implementations
// through the public entry and compares parse outcome, error layer, ordered
// values and visit locations.
func assertAgreement(t *testing.T, path string, doc interface{}) {
	t.Helper()
	assertAgreementCtx(t, context.Background(), path, doc)
}

func assertAgreementCtx(t *testing.T, ctx context.Context, path string, doc interface{}) {
	t.Helper()
	docJSON := jsonSnapshot(doc)

	eval, pErr := jsonpath.New(path)
	_, rParseErr := parseRefPath(path)
	if (pErr != nil) != (rParseErr != nil) {
		t.Fatalf("contract violation [parse layer] path=%s doc=%s\n  production parse error: %v\n  reference parse error: %v",
			path, docJSON, pErr, rParseErr)
	}
	if pErr != nil {
		return // both reject at parse layer
	}

	got, eErr := eval(ctx, doc)
	ref, rErr := refRun(ctx, path, doc)
	if (eErr != nil) != (rErr != nil) {
		t.Fatalf("contract violation [evaluation error layer] path=%s doc=%s\n  production error(%s): %v\n  reference error(%s): %v",
			path, docJSON, errorLayer(eErr), eErr, errorLayer(rErr), rErr)
	}
	if eErr != nil {
		if errorLayer(eErr) != errorLayer(rErr) {
			t.Fatalf("contract violation [fatal vs ordinary error] path=%s doc=%s\n  production: %v\n  reference: %v",
				path, docJSON, eErr, rErr)
		}
		return
	}

	if diff := cmp.Diff(ref.value, got); diff != "" {
		t.Fatalf("contract violation [ordered values] path=%s doc=%s\nreference(-) vs production(+):\n%s",
			path, docJSON, diff)
	}

	gotLocsRaw, locErr := evalWithPaths(ctx, path, doc)
	if locErr != nil {
		t.Fatalf("internal invariant broken: path diagnostic errored for a successful evaluation path=%s: %v",
			path, locErr)
	}
	gotLocs, ok := gotLocsRaw.(map[string]interface{})
	if !ok {
		t.Fatalf("internal invariant broken: diagnostic did not return a location map for %s: %#v",
			path, gotLocsRaw)
	}
	if violation := firstLocationViolation(ref.locs, gotLocs); violation != "" {
		t.Fatalf("contract violation [visit locations] path=%s doc=%s\n%s\nreference map=%#v\nproduction map=%#v",
			path, docJSON, violation, ref.locs, gotLocs)
	}
}

// ---------------- selector-level cases ----------------

func TestContract_NegativeIndexAndSliceEndpoints(t *testing.T) {
	// Every case is shaped so a single off-by-one at either slice endpoint,
	// at the negative-index translation or at the step boundary flips the
	// expected result.
	const arr = `["a","b","c"]`
	cases := []struct{ name, path string }{
		{"negative_index_last", "$[-1]"},
		{"negative_index_first", "$[-3]"},
		{"negative_index_out_of_range_returns_nil_terminal", "$[-4]"},
		{"positive_index_first_offbyone", "$[0]"},
		{"positive_index_last_offbyone", "$[2]"},
		{"slice_single_first_element_max_exclusive", "$[0:1]"},
		{"slice_two_elements_max_exclusive", "$[0:2]"},
		{"slice_max_equals_length", "$[1:3]"},
		{"slice_last_element_alone", "$[2:3]"},
		{"slice_empty_range", "$[3:3]"},
		{"slice_negative_start_to_end", "$[-2:]"},
		{"slice_start_to_negative_end_exclusive", "$[:-1]"},
		{"slice_both_negative_bounds", "$[-3:-1]"},
		{"slice_step_two_keeps_first_excludes_last", "$[0:3:2]"},
		{"slice_negative_step_full_reversal", "$[::-1]"},
		{"slice_reverse_min_greater_max_is_empty", "$[2:0:-1]"},
		{"slice_reverse_subrange", "$[1:3:-1]"},
		{"slice_bounds_beyond_length_clamp", "$[5:10]"},
		{"slice_negative_start_beyond_length_clamps_to_zero", "$[-5:2]"},
		{"slice_full_with_colons", "$[:]"},
		{"union_positive_endpoints", "$[0,2]"},
		{"union_positive_and_negative_endpoint", "$[0,-1]"},
		{"union_negative_out_of_range_emits_nil_then_b", "$[-5,1]"},
		{"terminal_positive_out_of_range_returns_nil", "$[5]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertAgreement(t, tc.path, mustJSONDoc(t, arr))
		})
	}
}

func TestContract_ObjectWildcardOrder(t *testing.T) {
	doc := mustJSONDoc(t, `{"b":1,"a":2,"c":3}`)
	for _, path := range []string{`$.*`, `$["a","c"]`, `$[*]`} {
		t.Run(path, func(t *testing.T) {
			assertAgreement(t, path, doc)
		})
	}
	// Object traversal must be reproducible: run the wildcard twenty times
	// and demand identical ordered results and locations.
	eval := mustNew(t, `$.*`)
	var first, firstLocs interface{}
	for i := 0; i < 20; i++ {
		got, err := eval(context.Background(), doc)
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		locs, err := evalWithPaths(context.Background(), `$.*`, doc)
		if err != nil {
			t.Fatalf("run %d locs: %v", i, err)
		}
		if i == 0 {
			first, firstLocs = got, locs
			continue
		}
		if !reflect.DeepEqual(first, got) {
			t.Fatalf("contract violation [reproducible object order] run %d diverged:\nfirst=%#v\ngot  =%#v",
				i, first, got)
		}
		if !reflect.DeepEqual(firstLocs, locs) {
			t.Fatalf("contract violation [reproducible object locations] run %d diverged:\nfirst=%#v\ngot  =%#v",
				i, firstLocs, locs)
		}
	}
}

func TestContract_RecursiveDescentBudgetAndOrder(t *testing.T) {
	doc := mustJSONDoc(t, `{
		"a": {"x": 1},
		"b": [{"x": 2}, {"y": 3}],
		"x": 4
	}`)
	t.Run("descent_values_and_locations", func(t *testing.T) {
		assertAgreement(t, "$..x", doc)
		assertAgreement(t, "$..[0]", doc)
	})
	t.Run("descent_wildcard_numeric_keys_sorted_numerically", func(t *testing.T) {
		assertAgreement(t, "$..*", mustJSONDoc(t, `{"10":"ten","2":"two"}`))
	})

	deep := buildDeepNested(12, "a")
	t.Run("visit_budget_exhaustion_is_fatal_and_recoverable", func(t *testing.T) {
		eval := mustNew(t, "$..a")
		limited := context.WithValue(context.Background(), jsonpath.VisitBudgetContextKey{}, 5)
		// Reference and production must agree on the same fatal cause.
		assertAgreementCtx(t, limited, "$..a", deep)
		if _, err := eval(limited, deep); !errors.Is(err, jsonpath.ErrVisitBudgetExceeded) {
			t.Fatalf("want ErrVisitBudgetExceeded, got %v", err)
		}
		// Recoverable failure: after the budget-blowing call, an unlimited
		// call through the reused evaluator succeeds fully.
		got, err := eval(context.Background(), deep)
		if err != nil {
			t.Fatalf("recovery call failed: %v", err)
		}
		fresh, err := jsonpath.Get("$..a", deep)
		if err != nil {
			t.Fatalf("fresh parse failed: %v", err)
		}
		if !reflect.DeepEqual(fresh, got) {
			t.Fatalf("reused evaluator carries state from failed budget call:\nfresh=%#v\nreused=%#v",
				fresh, got)
		}
	})
	t.Run("visit_budget_is_per_evaluation_not_cached", func(t *testing.T) {
		eval := mustNew(t, "$..a")
		limited := context.WithValue(context.Background(), jsonpath.VisitBudgetContextKey{}, 5)
		if _, err := eval(limited, deep); !errors.Is(err, jsonpath.ErrVisitBudgetExceeded) {
			t.Fatalf("want ErrVisitBudgetExceeded, got %v", err)
		}
		if _, err := eval(limited, deep); !errors.Is(err, jsonpath.ErrVisitBudgetExceeded) {
			t.Fatalf("budget leaked between evaluations: second limited call got %v", err)
		}
	})
	t.Run("cancellation_preserves_cause", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := mustNew(t, "$..a")(ctx, deep); !errors.Is(err, context.Canceled) {
			t.Fatalf("want context.Canceled causal error, got %v", err)
		}
		assertAgreementCtx(t, ctx, "$..a", deep)
	})
	t.Run("deadline_preserves_cause", func(t *testing.T) {
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Hour))
		defer cancel()
		if _, err := mustNew(t, "$..a")(ctx, deep); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("want context.DeadlineExceeded causal error, got %v", err)
		}
	})
}

func TestContract_DeepEmptyCollections(t *testing.T) {
	doc := mustJSONDoc(t, `{"a":{"b":{},"c":[]},"d":[]}`)
	assertAgreement(t, "$..*", doc)
	assertAgreement(t, "$..b", doc)
	assertAgreement(t, "$..missing", doc)
	assertAgreement(t, "$[*].*", doc)
	assertAgreement(t, "$..[*]", doc)
}

func TestContract_TypeMismatchesAreOrdinaryErrors(t *testing.T) {
	cases := []struct {
		name      string
		path      string
		doc       string
		wantPiece string
	}{
		{"string_key_on_array", "$.a.b", `{"a":[1,2]}`, "could not select value"},
		{"member_on_string", `$[0].x`, `["a"]`, "unsupported value type string"},
		{"missing_object_key", "$.missing", `{"a":1}`, "unknown key missing"},
		{"index_key_missing_on_object", "$[1]", `{"a":1}`, "unknown key 1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := mustJSONDoc(t, tc.doc)
			_, err := jsonpath.Get(tc.path, doc)
			if err == nil {
				t.Fatalf("expected ordinary evaluation error, got success")
			}
			if !contains(err.Error(), tc.wantPiece) {
				t.Fatalf("error layer/message mismatch: got %q, want piece %q", err.Error(), tc.wantPiece)
			}
			// The reference model must reject the same input at the same
			// (ordinary evaluation) layer.
			if _, rErr := refRun(context.Background(), tc.path, doc); rErr == nil {
				t.Fatalf("reference accepted an input production rejects: %s -> %v", tc.path, err)
			} else if errorLayer(rErr) != "evaluation" {
				t.Fatalf("reference error layer = %s, want evaluation: %v", errorLayer(rErr), rErr)
			}
			// Rejection is reproducible.
			if _, err2 := jsonpath.Get(tc.path, doc); err2 == nil || err2.Error() != err.Error() {
				t.Fatalf("error not reproducible: first=%q second=%v", err, err2)
			}
		})
	}
}

// ---------------- full path-level composition ----------------

func TestContract_FullPathComposition(t *testing.T) {
	cases := []struct {
		name string
		path string
		doc  string
	}{
		{"slice_then_member", "$[1:3].a", `[{"a":1},{"a":2},{"a":3}]`},
		{"wildcard_then_member", "$[*].a", `[{"a":1},{"b":2},{"a":3}]`},
		{"union_then_wildcard", "$[0,2].*", `[{"x":1},{"skip":9},{"y":2}]`},
		{"descent_then_union", `$..["x","a"]`, `{"a":{"x":1},"b":[{"x":2},{"y":3}],"x":4}`},
		{"descent_then_slice", "$..v[0:2]", `{"v":[1,2,3],"w":{"v":[4,5,6]}}`},
		{"slice_then_slice", "$[0:5:2][1:3]", `[[10,11,12],[20,21,22],[30,31,32]]`},
		{"wildcard_over_nested_maps", "$.store.*.price", `{"store":{"a":{"price":1},"b":{"price":2}}}`},
		{"descent_into_nested_arrays", `$..id`, `{"items":[{"id":1,"sub":{"id":2}},{"id":3}]}`},
		{"union_of_object_members_then_member", `$["u","v"].n`, `{"u":{"n":"u1"},"v":{"n":"v1"},"w":{"n":"w1"}}`},
		{"double_descent", "$..a..b", `{"a":{"b":1,"c":{"b":2}},"x":{"a":{"b":3}}}`},
		{"descent_negative_index", "$..[-1]", `{"g":[{"k":1},{"k":2}],"h":[{"k":3}]}`},
		{"plain_chain_mixed_brackets", `$["u"].n`, `{"u":{"n":"u1"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertAgreement(t, tc.path, mustJSONDoc(t, tc.doc))
		})
	}
}

// ---------------- syntax boundary: both reject ----------------

func TestContract_UnsupportedOrBrokenSyntaxRejectedByBothLayers(t *testing.T) {
	paths := []struct {
		name string
		path string
	}{
		{"empty", ""},
		{"no_root", ".a"},
		{"dangling_descent", "$.."},
		{"four_part_slice", "$[1:2:3:4]"},
		{"single_quoted_member", "$['a']"},
		{"mixed_slice_union", "$[1:2,3]"},
		{"mixed_union_slice", "$[1,2:3]"},
		{"trailing_dot", "$.a."},
		{"triple_dot", "$...a"},
		{"empty_bracket", "$[]"},
		{"dot_quoted", `$."a"`},
	}
	for _, tc := range paths {
		t.Run(tc.name, func(t *testing.T) {
			_, pErr := jsonpath.New(tc.path)
			_, rErr := parseRefPath(tc.path)
			if pErr == nil {
				t.Fatalf("production accepted path %q, expected parse rejection", tc.path)
			}
			if rErr == nil {
				t.Fatalf("reference model accepted path %q outside its subset", tc.path)
			}
			var pe *refParseError
			if !errors.As(rErr, &pe) {
				t.Fatalf("reference rejection must be a parse-layer error for %q, got %v", tc.path, rErr)
			}
			// Parse rejection must be reproducible.
			if _, err2 := jsonpath.New(tc.path); err2 == nil || err2.Error() != pErr.Error() {
				t.Fatalf("parse error not reproducible for %q:\nfirst=%v\nsecond=%v", tc.path, pErr, err2)
			}
		})
	}
}

// ---------------- syntax boundary: reference explicitly refuses extra features ----------------

func TestContract_ReferenceExplicitlyRejectsSyntaxOutsideSubset(t *testing.T) {
	// These paths are legal for the repository (optionally with extensions)
	// but the reference model must explicitly refuse them instead of
	// silently guessing: filters, scripts, placeholder object syntax.
	doc := mustJSONDoc(t, `{"a":[{"k":"x","v":1},{"k":"y","v":2}]}`)
	repoOK := []struct {
		name string
		lang gval.Language
		path string
	}{
		{"filter", jsonpath.Language(), `$..a[?@.k=="x"]`},
		{"script", jsonpath.Language(), `$.a[*].v(@==1)`},
		{"placeholder_object", jsonpath.PlaceholderExtension(), `{#0: $.a[*].k}`},
	}
	for _, tc := range repoOK {
		t.Run(tc.name, func(t *testing.T) {
			eval, err := tc.lang.NewEvaluable(tc.path)
			if err != nil {
				t.Fatalf("production must accept %q: %v", tc.path, err)
			}
			if _, err := eval(context.Background(), doc); err != nil {
				t.Fatalf("production must evaluate %q: %v", tc.path, err)
			}
			_, rErr := refRun(context.Background(), tc.path, doc)
			if !errors.Is(rErr, errRefUnsupported) {
				t.Fatalf("reference must explicitly reject %q with errRefUnsupported, got: %v",
					tc.path, rErr)
			}
		})
	}
}

// ---------------- placeholder binding ----------------

func TestContract_PlaceholderArityFailureIsIsolated(t *testing.T) {
	lang := jsonpath.PlaceholderExtension()
	mk := func(t *testing.T, p string) gval.Evaluable {
		t.Helper()
		eval, err := lang.NewEvaluable(p)
		if err != nil {
			t.Fatalf("parse %s: %v", p, err)
		}
		return eval
	}
	doc := func(a, b []interface{}) interface{} {
		return obj{"a": arr(a), "b": arr(b)}
	}

	t.Run("missing_wildcard_parameter_is_ordinary_stable_error", func(t *testing.T) {
		eval := mk(t, `{#1: $.a[*]}`)
		d := doc([]interface{}{"x", "y"}, nil)
		for i := 0; i < 3; i++ {
			got, err := eval(context.Background(), d)
			if err == nil {
				t.Fatalf("call %d: expected placeholder arity error, got %#v", i, got)
			}
			if !contains(err.Error(), "JSONPath placeholder #1 is not available") {
				t.Fatalf("call %d: wrong placeholder error layer/message: %v", i, err)
			}
		}
	})

	t.Run("failed_selector_does_not_pollute_second_entry_or_reuse", func(t *testing.T) {
		// First entry always binds wildcard #0; second entry asks for #1
		// although its selector produces one wildcard level, so the second
		// selector fails when b is non-empty. A single selector failure must
		// not poison the first entry's placeholder parameters.
		eval := mk(t, `{#0: $.a[*], #1: $.b[*]}`)

		bad := doc([]interface{}{"x", "y"}, []interface{}{"p"})
		if _, err := eval(context.Background(), bad); err == nil {
			t.Fatalf("expected placeholder failure for second entry")
		} else if !contains(err.Error(), "#1") {
			t.Fatalf("failure must name the failing placeholder #1: %v", err)
		}

		good := doc([]interface{}{"x", "y"}, nil)
		want := map[string]interface{}{"0": "x", "1": "y"}
		got, err := eval(context.Background(), good)
		if err != nil {
			t.Fatalf("reused evaluator must succeed after failed entry: %v", err)
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Fatalf("first entry parameters polluted by failed second entry:\n%s", diff)
		}
		// Retry with identical input: stable result.
		again, err := eval(context.Background(), good)
		if err != nil || !reflect.DeepEqual(again, got) {
			t.Fatalf("retry after failed selector not stable: %#v %v", again, err)
		}
	})

	t.Run("reused_placeholder_evaluator_switches_documents_cleanly", func(t *testing.T) {
		eval := mk(t, `{#0: $.a[*]}`)
		d1 := doc([]interface{}{"x", "y"}, nil)
		d2 := doc([]interface{}{"p"}, nil)
		r1, err := eval(context.Background(), d1)
		if err != nil {
			t.Fatalf("d1: %v", err)
		}
		if _, err := eval(context.Background(), d2); err != nil {
			t.Fatalf("d2: %v", err)
		}
		r1Again, err := eval(context.Background(), d1)
		if err != nil {
			t.Fatalf("d1 retry: %v", err)
		}
		if !reflect.DeepEqual(r1, r1Again) {
			t.Fatalf("placeholder state leaked between document switches:\nfirst=%#v\nlast =%#v", r1, r1Again)
		}
	})

	t.Run("placeholder_outside_object_key_rejected_at_parse", func(t *testing.T) {
		if _, err := lang.NewEvaluable(`#0`); err == nil {
			t.Fatalf("placeholder outside JSON object key must be rejected")
		}
	})
}

// ---------------- reused evaluator, diagnostic invariance ----------------

func TestContract_ReusedEvaluatorAndDiagnosticsDoNotMutateResults(t *testing.T) {
	path := `$..a[*]`
	doc1 := mustJSONDoc(t, `{"a":[{"k":1}],"b":{"a":[{"k":2}]}}`)
	doc2 := mustJSONDoc(t, `{"a":[{"k":3},{"k":4}]}`)
	eval := mustNew(t, path)

	baseline, err := eval(context.Background(), doc1)
	if err != nil {
		t.Fatal(err)
	}
	baselineLocs, err := evalWithPaths(context.Background(), path, doc1)
	if err != nil {
		t.Fatal(err)
	}
	// Alternate documents, diagnostic mode, and a failed budget call in the
	// middle; the final plain result for doc1 must equal the baseline exactly.
	if _, err := eval(context.Background(), doc2); err != nil {
		t.Fatal(err)
	}
	if _, err := evalWithPaths(context.Background(), path, doc2); err != nil {
		t.Fatal(err)
	}
	limited := context.WithValue(context.Background(), jsonpath.VisitBudgetContextKey{}, 1)
	if _, err := eval(limited, doc1); !errors.Is(err, jsonpath.ErrVisitBudgetExceeded) {
		t.Fatalf("budget failure expected, got %v", err)
	}
	after, err := eval(context.Background(), doc1)
	if err != nil {
		t.Fatalf("evaluation after interleave: %v", err)
	}
	if !reflect.DeepEqual(baseline, after) {
		t.Fatalf("diagnostics/failures mutated subsequent success:\nbaseline=%#v\nafter   =%#v", baseline, after)
	}
	afterLocs, err := evalWithPaths(context.Background(), path, doc1)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(baselineLocs, afterLocs) {
		t.Fatalf("location diagnostics changed between runs:\nfirst=%#v\nafter=%#v", baselineLocs, afterLocs)
	}
	// A sufficient diagnostic budget must not alter the success return.
	sufficient := context.WithValue(context.Background(), jsonpath.VisitBudgetContextKey{}, 1000)
	withBudget, err := eval(sufficient, doc1)
	if err != nil {
		t.Fatalf("sufficient budget: %v", err)
	}
	if !reflect.DeepEqual(baseline, withBudget) {
		t.Fatalf("diagnostic budget information changed successful result:\nbaseline=%#v\nbudget  =%#v", baseline, withBudget)
	}
}

// ---------------- acceptance story: mid-run recoverable failure ----------------

func TestContract_AcceptanceStory_MidRunFailureIsolationAndRetry(t *testing.T) {
	// Phase 1: build combined selector-level + full-path-level material and
	// establish reference agreement as the baseline.
	doc := buildDeepNested(8, "a")
	doc["b"] = []interface{}{
		map[string]interface{}{"a": []interface{}{1, 2}},
		map[string]interface{}{"a": []interface{}{3}},
	}
	path := `$..a[*]`
	placeholderPath := `{#0: $..a[*]}`

	assertAgreement(t, path, doc)
	baseline, err := jsonpath.Get(path, doc)
	if err != nil {
		t.Fatalf("baseline: %v", err)
	}
	baselineLocs, err := jsonpath.GetWithPaths(path, doc)
	if err != nil {
		t.Fatalf("baseline locs: %v", err)
	}
	snapshot := jsonSnapshot(doc)

	// Phase 2: mid-run, inject recoverable failures through the same public
	// entry: exhausted budget, cancellation; plus a placeholder evaluation.
	eval := mustNew(t, path)
	limited := context.WithValue(context.Background(), jsonpath.VisitBudgetContextKey{}, 4)
	if v, err := eval(limited, doc); !errors.Is(err, jsonpath.ErrVisitBudgetExceeded) {
		t.Fatalf("phase2 budget: want ErrVisitBudgetExceeded, got value=%#v err=%v", v, err)
	} else if v != nil {
		t.Fatalf("phase2 budget: fatal failure must not return a partial success value, got %#v", v)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := eval(ctx, doc); !errors.Is(err, context.Canceled) {
		t.Fatalf("phase2 cancel: want context.Canceled, got %v", err)
	}
	ph, err := jsonpath.PlaceholderExtension().NewEvaluable(placeholderPath)
	if err != nil {
		t.Fatalf("phase2 placeholder parse: %v", err)
	}
	if _, err := ph(limited, doc); !errors.Is(err, jsonpath.ErrVisitBudgetExceeded) {
		t.Fatalf("phase2 placeholder budget: want ErrVisitBudgetExceeded, got %v", err)
	}

	// Phase 3: no shared/persistent state was half-updated: the input
	// document is unchanged and the evaluator is still usable.
	if got := jsonSnapshot(doc); got != snapshot {
		t.Fatalf("shared input document mutated by failed evaluation:\nbefore=%s\nafter =%s", snapshot, got)
	}

	// Phase 4: retry with exactly the same inputs gives stable results via
	// the reused evaluator and via a fresh parse.
	for i := 0; i < 2; i++ {
		again, err := eval(context.Background(), doc)
		if err != nil {
			t.Fatalf("retry %d: %v", i, err)
		}
		if !reflect.DeepEqual(baseline, again) {
			t.Fatalf("retry %d unstable:\nbaseline=%#v\nagain   =%#v", i, baseline, again)
		}
		againLocs, err := evalWithPaths(context.Background(), path, doc)
		if err != nil || !reflect.DeepEqual(baselineLocs, againLocs) {
			t.Fatalf("retry %d locations unstable: %#v %v", i, againLocs, err)
		}
	}
	fresh, err := jsonpath.Get(path, doc)
	if err != nil || !reflect.DeepEqual(baseline, fresh) {
		t.Fatalf("fresh parse after failures diverged: baseline=%#v fresh=%#v err=%v", baseline, fresh, err)
	}
	phResult, err := ph(context.Background(), doc)
	if err != nil {
		t.Fatalf("placeholder recovery: %v", err)
	}
	if _, ok := phResult.(map[string]interface{}); !ok {
		t.Fatalf("placeholder recovery returned wrong shape: %#v", phResult)
	}
}
