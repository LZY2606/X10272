package jsonpath_test

// Regression materials for:
//   "jsonpath: selector combination, placeholder binding and failure isolation"
//
// Every case enters through the repository's real public entry points
// (jsonpath.Get / jsonpath.GetWithPaths / jsonpath.Language /
// jsonpath.PlaceholderExtension). Successful selectors are cross-checked
// value-by-value and access-location-by-access-location against an
// independent restricted reference interpreter (restricted_reference_test.go)
// that is fed only the parser input string itself. Failures must keep the
// repository's existing error layers and be repeatable.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/PaesslerAG/gval"
	"github.com/PaesslerAG/jsonpath"
)

type combinationCase struct {
	name string
	path string
	data string
	// wantValue is the observable result from jsonpath.Get.
	wantValue interface{}
	// wantErrSubstr pins the existing evaluation error layer/message; empty
	// means success.
	wantErrSubstr string
	// wantParseErr pins the existing parse error layer.
	wantParseErr bool
	// wantPaths pins GetWithPaths; nil skips the access-location comparison.
	wantPaths map[string]interface{}
	// noReference excludes this case from the independent interpreter (e.g.
	// scripts or placeholders which the reference deliberately rejects).
	noReference bool
}

func fixedCombinationCases() []combinationCase {
	return []combinationCase{
		{
			name:      "contract_negative_index_last",
			path:      "$[-1]",
			data:      `[7,"hey"]`,
			wantValue: "hey",
			wantPaths: map[string]interface{}{`$["-1"]`: "hey"},
		},
		{
			name:      "contract_negative_index_out_of_bounds",
			path:      "$[-2]",
			data:      `[7]`,
			wantValue: nil,
			wantPaths: map[string]interface{}{`$["-2"]`: nil},
		},
		{
			name:      "contract_slice_open_end_offbyone",
			path:      "$[:-1]",
			data:      `[0,1,2,3]`,
			wantValue: []interface{}{0., 1., 2.},
			wantPaths: map[string]interface{}{
				`$["0"]`: 0.,
				`$["1"]`: 1.,
				`$["2"]`: 2.,
			},
		},
		{
			name:      "contract_slice_open_start_negative",
			path:      "$[-2:]",
			data:      `[0,1,2,3]`,
			wantValue: []interface{}{2., 3.},
			wantPaths: map[string]interface{}{
				`$["2"]`: 2.,
				`$["3"]`: 3.,
			},
		},
		{
			name:      "contract_slice_full_step_minus_one",
			path:      "$[::-1]",
			data:      `[0,1,2,3]`,
			wantValue: []interface{}{3., 2., 1., 0.},
			wantPaths: map[string]interface{}{
				`$["3"]`: 3.,
				`$["2"]`: 2.,
				`$["1"]`: 1.,
				`$["0"]`: 0.,
			},
		},
		{
			name:      "contract_slice_reversed_clamped_max_offbyone",
			path:      "$[2:6:-1]",
			data:      `[0,1,2,3]`,
			wantValue: []interface{}{3., 2.},
			wantPaths: map[string]interface{}{
				`$["3"]`: 3.,
				`$["2"]`: 2.,
			},
		},
		{
			name:      "contract_slice_empty_when_endpoints_equal",
			path:      "$[0:0]",
			data:      `[0,1,2]`,
			wantValue: []interface{}{},
			wantPaths: map[string]interface{}{},
		},
		{
			name:      "contract_slice_start_beyond_end",
			path:      "$[10:5]",
			data:      `[0,1,2]`,
			wantValue: []interface{}{},
			wantPaths: map[string]interface{}{},
		},
		{
			name:      "contract_slice_on_object_is_empty",
			path:      "$[0:2]",
			data:      `{"a":1}`,
			wantValue: []interface{}{},
			wantPaths: map[string]interface{}{},
		},
		{
			name:      "contract_object_wildcard_order_is_reproducible",
			path:      "$[*]",
			data:      `{"zeta":1,"alpha":2,"mid":3}`,
			wantValue: []interface{}{2., 3., 1.},
			wantPaths: map[string]interface{}{
				`$["alpha"]`: 2.,
				`$["mid"]`:   3.,
				`$["zeta"]`:  1.,
			},
		},
		{
			name:      "contract_wildcard_on_scalar_is_empty",
			path:      "$[*]",
			data:      `5`,
			wantValue: []interface{}{},
			wantPaths: map[string]interface{}{},
		},
		{
			name:      "contract_recursive_descent_preorder_sorted",
			path:      "$..x",
			data:      `{"x":1,"o":{"x":2,"p":{"x":3}}}`,
			wantValue: []interface{}{1., 2., 3.},
			wantPaths: map[string]interface{}{
				`$["x"]`:           1.,
				`$["o"]["x"]`:      2.,
				`$["o"]["p"]["x"]`: 3.,
			},
		},
		{
			name:      "contract_recursive_descent_empty_object",
			path:      "$..x",
			data:      `{}`,
			wantValue: []interface{}{},
			wantPaths: map[string]interface{}{},
		},
		{
			name:      "contract_recursive_descent_wildcard_empty_array",
			path:      "$..*",
			data:      `[]`,
			wantValue: []interface{}{},
			wantPaths: map[string]interface{}{},
		},
		{
			name:      "contract_deep_empty_collections",
			path:      "$.a.b[*].c",
			data:      `{"a":{"b":[{"d":1},{},[]]}}`,
			wantValue: []interface{}{},
			wantPaths: map[string]interface{}{},
		},
		{
			name:          "contract_type_mismatch_index_on_object",
			path:          "$[0]",
			data:          `{"a":1}`,
			wantErrSubstr: "unknown key 0",
			noReference:   false,
		},
		{
			name:          "contract_type_mismatch_key_on_array",
			path:          "$.k",
			data:          `[1,2,3]`,
			wantErrSubstr: "could not select value, invalid key: expected number but got k (string)",
		},
		{
			name:          "contract_type_mismatch_select_on_string",
			path:          "$[0]",
			data:          `"str"`,
			wantErrSubstr: "unsupported value type string for select",
		},
		{
			name:      "contract_union_negative_index_keeps_hole",
			path:      "$[1,-5]",
			data:      `[55,{"a":"1a"},70,{"a":"bb"}]`,
			wantValue: []interface{}{map[string]interface{}{"a": "1a"}, nil},
			wantPaths: map[string]interface{}{
				`$["1"]`:  map[string]interface{}{"a": "1a"},
				`$["-5"]`: nil,
			},
		},
		{
			name:         "contract_parse_error_incomplete",
			path:         "$[3].",
			wantParseErr: true,
		},
	}
}

func mustJSON(t *testing.T, data string) interface{} {
	t.Helper()
	var v interface{}
	if err := json.Unmarshal([]byte(data), &v); err != nil {
		t.Fatalf("broken fixture json %q: %v", data, err)
	}
	return v
}

// firstViolation reports the first position where actual diverges from
// expected, including the necessary context instead of a bare bool.
func firstViolation(label string, want, got interface{}) string {
	if reflect.DeepEqual(want, got) {
		return ""
	}
	return fmt.Sprintf("%s first violation:\n  want: %s\n  got:  %s",
		label, describeValue(want), describeValue(got))
}

func describeValue(v interface{}) string {
	switch t := v.(type) {
	case []interface{}:
		parts := make([]string, len(t))
		for i, e := range t {
			parts[i] = describeValue(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]interface{}:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%q:%s", k, describeValue(t[k])))
		}
		return "map{" + strings.Join(parts, ", ") + "}"
	default:
		return fmt.Sprintf("%#v", v)
	}
}

func runCombinationCase(t *testing.T, tc combinationCase, lang gval.Language, data interface{}) {
	t.Helper()
	eval, err := lang.NewEvaluable(tc.path)
	if tc.wantParseErr {
		if err == nil {
			t.Fatalf("[%s] expected parse error at the parse layer, got evaluable", tc.name)
		}
		if !strings.Contains(err.Error(), "parsing error") {
			t.Fatalf("[%s] rejection must stay in parse error layer, got: %v", tc.name, err)
		}
		return
	}
	if err != nil {
		t.Fatalf("[%s] unexpected parse error: %v", tc.name, err)
	}

	got, gerr := eval(context.Background(), data)
	if tc.wantErrSubstr != "" {
		if gerr == nil {
			t.Fatalf("[%s] expected evaluation error containing %q, got %#v", tc.name, tc.wantErrSubstr, got)
		}
		if !strings.Contains(gerr.Error(), tc.wantErrSubstr) {
			t.Fatalf("[%s] error layer mismatch:\n  want substr: %q\n  got: %q", tc.name, tc.wantErrSubstr, gerr.Error())
		}
		return
	}
	if gerr != nil {
		t.Fatalf("[%s] unexpected evaluation error: %v", tc.name, gerr)
	}
	if msg := firstViolation("["+tc.name+"] value", tc.wantValue, got); msg != "" {
		t.Fatal(msg)
	}

	if tc.wantPaths != nil {
		gotPaths, perr := jsonpath.GetWithPaths(tc.path, data)
		if perr != nil {
			t.Fatalf("[%s] GetWithPaths error: %v", tc.name, perr)
		}
		if msg := firstViolation("["+tc.name+"] access locations", tc.wantPaths, gotPaths); msg != "" {
			t.Fatal(msg)
		}
	}
}

// TestSelectorCombinationFixed is the fixed-input group covering negative
// indices and slices, object wildcards, recursive descent, type mismatches
// and deep empty collections. Successful cases are additionally checked
// against the independent restricted reference interpreter.
func TestSelectorCombinationFixed(t *testing.T) {
	lang := jsonpath.Language()
	for _, tc := range fixedCombinationCases() {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			var data interface{}
			if tc.data != "" {
				data = mustJSON(t, tc.data)
			}
			runCombinationCase(t, tc, lang, data)
			if !tc.noReference && !tc.wantParseErr && tc.wantErrSubstr == "" {
				assertReferenceAgrees(t, tc.path, data)
			}
			if tc.wantErrSubstr != "" && !tc.noReference {
				assertReferenceErrorAgrees(t, tc.path, data, tc.wantErrSubstr)
			}
		})
	}
}

// assertReferenceAgrees runs the same JSONPath through the public
// jsonpath.Get / GetWithPaths entries and through the independent restricted
// reference interpreter, comparing both values and access locations. The
// reference only receives the parser input string, never internal objects.
func assertReferenceAgrees(t *testing.T, path string, data interface{}) {
	t.Helper()
	steps, err := parseReferencePath(path)
	if err != nil {
		t.Fatalf("reference rejected a repository-supported path %q: %v", path, err)
	}
	refValue, refPaths, err := evalReference(data, steps)
	if err != nil {
		t.Fatalf("reference failed on a repository-supported path %q: %v", path, err)
	}
	gotValue, err := jsonpath.Get(path, data)
	if err != nil {
		t.Fatalf("repository failed on a reference-supported path %q: %v", path, err)
	}
	if msg := firstViolation("["+path+"] reference value", refValue, gotValue); msg != "" {
		t.Fatal(msg)
	}
	gotPaths, err := jsonpath.GetWithPaths(path, data)
	if err != nil {
		t.Fatalf("GetWithPaths failed: %v", err)
	}
	if msg := firstViolation("["+path+"] reference access locations", refPaths, gotPaths); msg != "" {
		t.Fatal(msg)
	}
}

func assertReferenceErrorAgrees(t *testing.T, path string, data interface{}, wantSubstr string) {
	t.Helper()
	steps, err := parseReferencePath(path)
	if err != nil {
		t.Fatalf("reference rejected a repository-supported path %q: %v", path, err)
	}
	_, _, rerr := evalReference(data, steps)
	if rerr == nil {
		t.Fatalf("[%s] reference did not reproduce error containing %q", path, wantSubstr)
	}
	if !strings.Contains(rerr.Error(), wantSubstr) {
		t.Fatalf("[%s] reference error %q does not contain %q", path, rerr.Error(), wantSubstr)
	}
}

// TestRestrictedReferenceRejectsUnsupportedSyntax proves the reference model
// only covers the repository-supported subset: every listed syntax is
// rejected by both the reference and the public jsonpath parser, with the
// repository rejection staying in its parse error layer and being repeatable.
func TestRestrictedReferenceRejectsUnsupportedSyntax(t *testing.T) {
	unsupported := []string{
		"$['key']",
		"$['missing']",
		"$..'key'",
		"$.2",
		"$[?(@.key>42 && @.key<44)]",
		"$[?(@.name=~/hello.*/)]",
		"$[?(@.a && @.b)]",
		"$[?(!@.key)]",
		"$..",
		"$.key..",
		"$[1:3,4]",
		"$[1,?true]",
	}
	lang := jsonpath.Language()
	for _, p := range unsupported {
		p := p
		t.Run(p, func(t *testing.T) {
			if _, err := parseReferencePath(p); err == nil {
				t.Fatalf("reference accepted out-of-subset syntax %q", p)
			}
			_, err1 := lang.NewEvaluable(p)
			_, err2 := lang.NewEvaluable(p)
			if err1 == nil {
				t.Fatalf("repository accepted syntax expected to be rejected: %q", p)
			}
			if !strings.Contains(err1.Error(), "parsing error") {
				t.Fatalf("rejection of %q must be a parse-layer error, got: %v", p, err1)
			}
			if err1.Error() != err2.Error() {
				t.Fatalf("rejection not repeatable for %q:\n  %v\n  %v", p, err1, err2)
			}
		})
	}
}

// TestEvaluatorReuseAndPlaceholderIsolation proves a single selector failure
// neither pollutes reused placeholder bindings nor leaves state behind on a
// reused evaluable: success after failure and failure after success must both
// stay stable under identical retries.
func TestEvaluatorReuseAndPlaceholderIsolation(t *testing.T) {
	const path = `{#0: $..x}`
	lang := jsonpath.PlaceholderExtension()
	eval, err := lang.NewEvaluable(path)
	if err != nil {
		t.Fatalf("parse placeholder path: %v", err)
	}

	successData := mustJSON(t, `{"a":{"x":1},"b":[{"x":2},{"y":3}],"x":4}`)
	wantSuccess := map[string]interface{}{
		`[]`:    4.,
		`[a]`:   1.,
		`[b 0]`: 2.,
	}
	failureData := mustJSON(t, `{"x":1}`)
	const wantFailure = "JSONPath placeholder #0 is not available"

	sequence := []struct {
		data interface{}
		want interface{}
		err  string
	}{
		{failureData, nil, wantFailure},
		{successData, wantSuccess, ""},
		{failureData, nil, wantFailure},
		{successData, wantSuccess, ""},
		{failureData, nil, wantFailure},
	}
	for i, step := range sequence {
		got, gerr := eval(context.Background(), step.data)
		if step.err != "" {
			if gerr == nil || !strings.Contains(gerr.Error(), step.err) {
				t.Fatalf("step %d: expected error %q, got %#v %v", i, step.err, got, gerr)
			}
			continue
		}
		if gerr != nil {
			t.Fatalf("step %d: unexpected error %v", i, gerr)
		}
		if msg := firstViolation(fmt.Sprintf("reuse step %d value", i), step.want, got); msg != "" {
			t.Fatal(msg)
		}
	}

	// within-call isolation: a failed child selector must not suppress or
	// corrupt sibling matches of the same reused evaluation.
	eval2, err := lang.NewEvaluable(`{#0: $[*].v}`)
	if err != nil {
		t.Fatal(err)
	}
	partial := mustJSON(t, `{"a":{"v":1},"b":{},"c":{"v":3}}`)
	got, gerr := eval2(context.Background(), partial)
	if gerr != nil {
		t.Fatalf("partial wildcard failure must be isolated, got error: %v", gerr)
	}
	wantPartial := map[string]interface{}{"a": 1., "c": 3.}
	if msg := firstViolation("within-call placeholder isolation", wantPartial, got); msg != "" {
		t.Fatal(msg)
	}

	// identical retry stability for plain selectors too
	plain, err := jsonpath.Language().NewEvaluable(`$[1,-5].a`)
	if err != nil {
		t.Fatal(err)
	}
	d := mustJSON(t, `[55,{"a":"1a"},70,{"a":"bb"}]`)
	for i := 0; i < 3; i++ {
		r, e := plain(context.Background(), d)
		if e != nil {
			t.Fatalf("retry %d: %v", i, e)
		}
		if msg := firstViolation(fmt.Sprintf("plain retry %d", i), []interface{}{"1a"}, r); msg != "" {
			t.Fatal(msg)
		}
	}
}

// TestRecursiveDescentBudgetAndCancellation covers the mid-story recoverable
// failure: budget exhaustion and context cancellation keep distinct causal
// errors, no partial result is returned, the same inputs repeat identically,
// and a later unconstrained evaluation recovers fully without leftover state.
func TestRecursiveDescentBudgetAndCancellation(t *testing.T) {
	deep := mustJSON(t, `{"x":1,"o":{"x":2,"p":{"x":3}}}`)
	const path = `$..x`
	lang := jsonpath.Language()
	eval, err := lang.NewEvaluable(path)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("budget_exhausted_is_causal_and_repeatable", func(t *testing.T) {
		ctx := context.WithValue(context.Background(), jsonpath.DescentBudgetContextKey{}, 2)
		var first error
		for i := 0; i < 3; i++ {
			r, e := eval(ctx, deep)
			var bErr *jsonpath.DescentBudgetExceededError
			if !errors.As(e, &bErr) {
				t.Fatalf("attempt %d: expected DescentBudgetExceededError, got %#v %v", i, r, e)
			}
			if bErr.Limit != 2 {
				t.Fatalf("attempt %d: budget limit diagnostic changed: %d", i, bErr.Limit)
			}
			if r != nil {
				t.Fatalf("attempt %d: failed descent must not return partial result: %#v", i, r)
			}
			if first == nil {
				first = e
			} else if first.Error() != e.Error() {
				t.Fatalf("budget failure not repeatable:\n  %v\n  %v", first, e)
			}
		}
	})

	t.Run("zero_budget_fails_immediately", func(t *testing.T) {
		ctx := context.WithValue(context.Background(), jsonpath.DescentBudgetContextKey{}, 0)
		r, e := eval(ctx, deep)
		var bErr *jsonpath.DescentBudgetExceededError
		if !errors.As(e, &bErr) || bErr.Limit != 0 {
			t.Fatalf("expected zero-budget DescentBudgetExceededError, got %#v %v", r, e)
		}
	})

	t.Run("cancellation_has_its_own_cause", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		r, e := eval(ctx, deep)
		if !errors.Is(e, context.Canceled) {
			t.Fatalf("expected context.Canceled cause, got %#v %v", r, e)
		}
		var bErr *jsonpath.DescentBudgetExceededError
		if errors.As(e, &bErr) {
			t.Fatalf("cancellation must not be misreported as budget exhaustion: %v", e)
		}
	})

	t.Run("recovery_without_leftover_counter", func(t *testing.T) {
		failCtx := context.WithValue(context.Background(), jsonpath.DescentBudgetContextKey{}, 1)
		if _, e := eval(failCtx, deep); e == nil {
			t.Fatal("expected budget failure before recovery")
		}
		r, e := eval(context.Background(), deep)
		if e != nil {
			t.Fatalf("recovered evaluation must succeed, got: %v", e)
		}
		if msg := firstViolation("post-budget recovery value", []interface{}{1., 2., 3.}, r); msg != "" {
			t.Fatal(msg)
		}
	})

	t.Run("successful_calls_keep_normal_diagnostic_shape", func(t *testing.T) {
		ctx := context.WithValue(context.Background(), jsonpath.DescentBudgetContextKey{}, 1000)
		r, e := eval(ctx, deep)
		if e != nil {
			t.Fatalf("diagnostic must not alter successful return: %v", e)
		}
		if msg := firstViolation("budgeted success value", []interface{}{1., 2., 3.}, r); msg != "" {
			t.Fatal(msg)
		}
	})
}

// TestOrderStableUnderScheduling runs the same deterministic-order cases in
// different execution orders, cache states and interleavings (goroutines).
// Only scheduling/enumeration conditions change; results must be identical.
func TestOrderStableUnderScheduling(t *testing.T) {
	cases := fixedCombinationCases()
	lang := jsonpath.Language()

	prepare := func(t *testing.T) []struct {
		tc   combinationCase
		data interface{}
	} {
		out := make([]struct {
			tc   combinationCase
			data interface{}
		}, 0, len(cases))
		for _, tc := range cases {
			if tc.wantParseErr || tc.wantErrSubstr != "" || tc.data == "" {
				continue
			}
			out = append(out, struct {
				tc   combinationCase
				data interface{}
			}{tc, mustJSON(t, tc.data)})
		}
		return out
	}

	// schedule A: original order, fresh evaluables
	t.Run("fresh_evaluables_forward", func(t *testing.T) {
		for _, c := range prepare(t) {
			runCombinationCase(t, c.tc, lang, c.data)
		}
	})

	// schedule B: reversed order, then forward again using one compiled set
	// (the second pass proves prior calls left no cached/permuted state)
	t.Run("compiled_reverse_then_forward", func(t *testing.T) {
		ready := prepare(t)
		compiled := make([]gval.Evaluable, len(ready))
		for i, c := range ready {
			e, err := lang.NewEvaluable(c.tc.path)
			if err != nil {
				t.Fatalf("[%s] %v", c.tc.name, err)
			}
			compiled[i] = e
		}
		for i := len(ready) - 1; i >= 0; i-- {
			got, err := compiled[i](context.Background(), ready[i].data)
			if err != nil {
				t.Fatalf("[%s] %v", ready[i].tc.name, err)
			}
			if msg := firstViolation("["+ready[i].tc.name+"] reverse-schedule value", ready[i].tc.wantValue, got); msg != "" {
				t.Fatal(msg)
			}
		}
		for i := range ready {
			got, err := compiled[i](context.Background(), ready[i].data)
			if err != nil {
				t.Fatalf("[%s] %v", ready[i].tc.name, err)
			}
			if msg := firstViolation("["+ready[i].tc.name+"] replayed value", ready[i].tc.wantValue, got); msg != "" {
				t.Fatal(msg)
			}
		}
	})

	// schedule C: concurrent interleaving of reused evaluables
	t.Run("concurrent_interleaving", func(t *testing.T) {
		ready := prepare(t)
		done := make(chan error, len(ready))
		for _, c := range ready {
			c := c
			go func() {
				e, err := lang.NewEvaluable(c.tc.path)
				if err != nil {
					done <- err
					return
				}
				for k := 0; k < 5; k++ {
					got, gerr := e(context.Background(), c.data)
					if gerr != nil {
						done <- gerr
						return
					}
					if !reflect.DeepEqual(c.tc.wantValue, got) {
						done <- fmt.Errorf("[%s] concurrent result drifted: %#v", c.tc.name, got)
						return
					}
				}
				done <- nil
			}()
		}
		for range ready {
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		}
	})
}

// TestSliceEndpointMutationBait contains shapes a typical off-by-one
// implementation gets wrong (end-exclusive slices, negative default bounds
// and reverse-step endpoint handling).
func TestSliceEndpointMutationBait(t *testing.T) {
	bait := []combinationCase{
		{
			name:      "mutation_bait_slice_drops_last_element",
			path:      "$[0:3]",
			data:      `[10,20,30,40]`,
			wantValue: []interface{}{10., 20., 30.},
			wantPaths: map[string]interface{}{
				`$["0"]`: 10., `$["1"]`: 20., `$["2"]`: 30.,
			},
		},
		{
			name:      "mutation_bait_single_element_slice",
			path:      "$[2:3]",
			data:      `[10,20,30,40]`,
			wantValue: []interface{}{30.},
			wantPaths: map[string]interface{}{`$["2"]`: 30.},
		},
		{
			name:      "mutation_bait_negative_end_must_exclude_last",
			path:      "$[1:-1]",
			data:      `[10,20,30,40]`,
			wantValue: []interface{}{20., 30.},
			wantPaths: map[string]interface{}{`$["1"]`: 20., `$["2"]`: 30.},
		},
		{
			name:      "mutation_bait_reverse_step_start_includes_clamped_end",
			path:      "$[0:-1:-1]",
			data:      `[10,20,30,40]`,
			wantValue: []interface{}{30., 20., 10.},
			wantPaths: map[string]interface{}{
				`$["2"]`: 30., `$["1"]`: 20., `$["0"]`: 10.,
			},
		},
	}
	for _, tc := range bait {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			data := mustJSON(t, tc.data)
			runCombinationCase(t, tc, jsonpath.Language(), data)
			assertReferenceAgrees(t, tc.path, data)
		})
	}
}

// TestSuccessfulResultsCarryNoSharedState proves repeated successes produce
// independent result containers (fresh slices/maps, no shared cache or
// counter) and that mutating one observed result does not change the next.
func TestSuccessfulResultsCarryNoSharedState(t *testing.T) {
	eval, err := jsonpath.Language().NewEvaluable(`$.items[*]`)
	if err != nil {
		t.Fatal(err)
	}
	data := mustJSON(t, `{"items":[{"v":1},{"v":2}]}`)

	r1, err := eval(context.Background(), data)
	if err != nil {
		t.Fatal(err)
	}
	s1 := r1.([]interface{})
	s1[0] = "POISONED"
	s1 = append(s1, "EXTRA")

	r2, err := eval(context.Background(), data)
	if err != nil {
		t.Fatal(err)
	}
	want := []interface{}{
		map[string]interface{}{"v": 1.},
		map[string]interface{}{"v": 2.},
	}
	if msg := firstViolation("second result must not carry first call's mutation", want, r2); msg != "" {
		t.Fatal(msg)
	}
	if len(r2.([]interface{})) != 2 {
		t.Fatalf("appended element leaked into fresh result: %#v", r2)
	}

	// with-paths maps must be independently allocated too
	p1, err := jsonpath.GetWithPaths(`$.items[*]`, data)
	if err != nil {
		t.Fatal(err)
	}
	p1.(map[string]interface{})["$[\"poison\"]"] = "x"
	p2, err := jsonpath.GetWithPaths(`$.items[*]`, data)
	if err != nil {
		t.Fatal(err)
	}
	if _, leaked := p2.(map[string]interface{})[`$["poison"]`]; leaked {
		t.Fatal("with-paths map carried state from a previous call")
	}
}

// TestPlaceholderUnavailableRepeats pins the placeholder insufficiency
// failure layer and proves it repeats identically through the public
// PlaceholderExtension entry.
func TestPlaceholderUnavailableRepeats(t *testing.T) {
	eval, err := jsonpath.PlaceholderExtension().NewEvaluable(`{#2: $[*]}`)
	if err != nil {
		t.Fatal(err)
	}
	data := mustJSON(t, `[10]`)
	var first error
	for i := 0; i < 3; i++ {
		r, e := eval(context.Background(), data)
		if e == nil || !strings.Contains(e.Error(), "JSONPath placeholder #2 is not available") {
			t.Fatalf("attempt %d: expected placeholder insufficiency error, got %#v %v", i, r, e)
		}
		if first == nil {
			first = e
		} else if first.Error() != e.Error() {
			t.Fatalf("placeholder failure not repeatable:\n  %v\n  %v", first, e)
		}
	}
	// the reusable evaluator still succeeds when the binding is provided
	ok, err := eval(context.Background(), mustJSON(t, `[10,20,30]`))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := ok.(map[string]interface{})[`$["2"]`], 30.; got != want {
		t.Fatalf("placeholder recovery mismatch: got %#v want %v", got, want)
	}
}

// TestFixtureFromTempDirectory exercises the same public entry with a
// fixture materialized in a temporary directory (no external services, no
// machine preset data).
func TestFixtureFromTempDirectory(t *testing.T) {
	dir := t.TempDir()
	fixture := filepath.Join(dir, "document.json")
	payload := `{"store":{"book":[{"category":"reference","price":8.95},{"category":"fiction","price":12.99}]}}`
	if err := os.WriteFile(fixture, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	var doc interface{}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	got, err := jsonpath.Get(`$.store.book[*].category`, doc)
	if err != nil {
		t.Fatal(err)
	}
	want := []interface{}{"reference", "fiction"}
	if msg := firstViolation("temp-dir fixture value", want, got); msg != "" {
		t.Fatal(msg)
	}
	paths, err := jsonpath.GetWithPaths(`$.store.book[*].category`, doc)
	if err != nil {
		t.Fatal(err)
	}
	wantPaths := map[string]interface{}{
		`$["store"]["book"]["0"]["category"]`: "reference",
		`$["store"]["book"]["1"]["category"]`: "fiction",
	}
	if msg := firstViolation("temp-dir fixture access locations", wantPaths, paths); msg != "" {
		t.Fatal(msg)
	}
}
