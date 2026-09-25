package jsonpath_test

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand"
	"reflect"
	"sync"
	"testing"

	"github.com/PaesslerAG/jsonpath"
)

// TestReferenceRejectsUnsupportedSyntax proves the restricted reference only
// covers the repository's supported subset and explicitly rejects everything
// else instead of silently producing a value. Rejection must be repeatable and
// land at the parse-error layer for queries the production parser also rejects.
func TestReferenceRejectsUnsupportedSyntax(t *testing.T) {
	rejected := []string{
		"$['single']",        // single quoted keys
		"$[?@.a==1]",         // filter
		"$[?(@.a==1)]",       // grouped filter
		"$.a[?true]",         // dot-bracket filter
		"$..length()",        // function
		"$.a(@+1)",           // script
		"$[?(@.d==['v1'])]",  // filter with array literal
		"$[?(!@.a)]",         // filter negation
		"$[?(@.name=~/.*/)]", // regex operator
		"@.a",                // current node path
		"$.a && $.b",         // boolean operator
		"...x",               // triple dot
		"$.",                 // dangling dot
		"$[1,5:1]",           // mixed union and slice
		`{#0: $[*]}`,         // placeholder object
	}
	for _, query := range rejected {
		query := query
		t.Run("reference_explicit_reject/"+sanitize(query), func(t *testing.T) {
			_, rerr := refParse(query)
			if !errors.Is(rerr, errRefRejected) {
				t.Fatalf("first_violation: reference accepted syntax outside its subset:\n  query: %s\n  got: %v", query, rerr)
			}
			// repeatability
			_, rerr2 := refParse(query)
			if rerr2.Error() != rerr.Error() {
				t.Fatalf("first_violation: reference rejection is not repeatable:\n  %q != %q", rerr, rerr2)
			}
			// The production parser must reject the same queries at the parse
			// layer (same error layering), except the placeholder object, which
			// needs the placeholder language and is isolated separately.
			if query == `{#0: $[*]}` {
				if _, err := jsonpath.New(query); err == nil {
					t.Fatalf("first_violation: base language accepted placeholder object %q", query)
				}
				return
			}
			if _, err := jsonpath.New(query); err == nil {
				t.Fatalf("first_violation: production parse error layer disagreement, accepted %q", query)
			}
		})
	}
}

// TestCompositionOrderStability reruns the fixed counter-example set in
// shuffled order, concurrently, and with different (irrelevant) budget
// settings. The observable values and access positions must stay identical;
// only scheduling conditions change.
func TestCompositionOrderStability(t *testing.T) {
	doc := loadCompositionDocument(t)
	cases := compositionCases()
	baseline := map[string][2]string{}
	for _, tc := range cases {
		ref := refEvaluate(tc.query, doc)
		eval, err := jsonpath.New(tc.query)
		if err != nil {
			t.Fatalf("%s: parse: %v", tc.name, err)
		}
		got, err := eval(context.Background(), doc)
		if err != nil {
			if ref.err == nil {
				t.Fatalf("%s: unexpected error %v", tc.name, err)
			}
			baseline[tc.name] = [2]string{"E:" + err.Error(), "E:" + ref.err.Error()}
			continue
		}
		sv, _ := stableValues(got)
		baseline[tc.name] = [2]string{mustJSON(sv), mustJSON(stableRef(ref))}
	}

	budgets := []int{-1, 0, 1, 100000}
	for round := 0; round < 4; round++ {
		rng := rand.New(rand.NewSource(int64(round) + 42))
		order := rng.Perm(len(cases))
		var wg sync.WaitGroup
		for _, idx := range order {
			tc := cases[idx]
			budget := budgets[(round+idx)%len(budgets)]
			wg.Add(1)
			go func() {
				defer wg.Done()
				ctx := context.WithValue(context.Background(), jsonpath.RecursiveDescentBudgetContextKey{}, budget)
				eval, err := jsonpath.Language().NewEvaluableWithContext(ctx, tc.query)
				if err != nil {
					t.Errorf("%s: parse on round %d: %v", tc.name, round, err)
					return
				}
				got, gerr := eval(ctx, doc)
				want := baseline[tc.name]
				if gerr != nil {
					if want[0] != "E:"+gerr.Error() {
						t.Errorf("%s: round %d first_violation: error drifted: %v", tc.name, round, gerr)
					}
					return
				}
				sv, _ := stableValues(got)
				if got1 := mustJSON(sv); got1 != want[0] {
					t.Errorf("%s: round %d first_violation: values changed with scheduling/budget\n  got: %s\n  want: %s",
						tc.name, round, got1, want[0])
				}
			}()
		}
		wg.Wait()
	}
}

func stableRef(ref refOutcome) interface{} {
	if ref.err != nil {
		return map[string]string{"error": ref.err.Error()}
	}
	if ref.ambiguous {
		return ref.values()
	}
	return ref.values()[0]
}

// TestObjectTraversalReproducible directly asserts the recursive descent and
// wildcard contract: object traversal is sorted and reproducible.
func TestObjectTraversalReproducible(t *testing.T) {
	var doc interface{}
	if err := json.Unmarshal([]byte(`{"b":1,"a":2,"c":{"z":3,"y":4}}`), &doc); err != nil {
		t.Fatal(err)
	}
	want := []interface{}{2., obj{"z": 3., "y": 4.}, 1.}
	descentWant := []interface{}{2., obj{"z": 3., "y": 4.}, 4., 3., 1.}
	eval, err := jsonpath.New(`$.*`)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 30; i++ {
		got, err := eval(context.Background(), doc)
		if err != nil {
			t.Fatalf("reproducible_star iteration %d: %v", i, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("reproducible_star_order first_violation at iteration %d:\n  got:  %s\n  want: %s",
				i, mustJSON(got), mustJSON(want))
		}
	}
	deval, err := jsonpath.New(`$..*`)
	if err != nil {
		t.Fatal(err)
	}
	first, err := deval(context.Background(), doc)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, descentWant) {
		t.Fatalf("reproducible_descent_order first_violation:\n  got:  %s\n  want: %s",
			mustJSON(first), mustJSON(descentWant))
	}
	for i := 0; i < 30; i++ {
		got, _ := deval(context.Background(), doc)
		if !reflect.DeepEqual(got, first) {
			t.Fatalf("reproducible_descent_order drifted at iteration %d", i)
		}
	}
}

// budgetTree builds a tree whose recursive descent visits exactly wantVisits
// values: 1 root + degree + degree*2 leaves, i.e. 1+d+2d = 1+3d.
func budgetTree(degree int) interface{} {
	root := obj{}
	children := arr{}
	for i := 0; i < degree; i++ {
		children = append(children, obj{"leaf1": 1, "leaf2": 2})
	}
	root["children"] = children
	return root
}

// TestRecursiveDescentBudget enforces the visit-budget contract including the
// exact boundary (the off-by-one killer: budget == visit count succeeds,
// budget == visit count-1 fails), recovery, and absence of leaked counters.
func TestRecursiveDescentBudget(t *testing.T) {
	doc := budgetTree(5) // 1 + 3*5 = 16 visited values
	const visits = 16
	eval, err := jsonpath.New(`$..*`)
	if err != nil {
		t.Fatal(err)
	}

	okCtx := context.WithValue(context.Background(), jsonpath.RecursiveDescentBudgetContextKey{}, visits)
	if _, err := eval(okCtx, doc); err != nil {
		t.Fatalf("descent_budget_at_exact_boundary first_violation: expected success at budget %d, got %v", visits, err)
	}
	tightCtx := context.WithValue(context.Background(), jsonpath.RecursiveDescentBudgetContextKey{}, visits-1)
	if _, err := eval(tightCtx, doc); !errors.Is(err, jsonpath.ErrRecursiveDescentBudgetExhausted) {
		t.Fatalf("descent_budget_one_below_boundary first_violation: expected budget exhaustion, got %v", err)
	}
	// Recoverable failure: the same reusable evaluable succeeds afterwards
	// with an adequate budget. No counter from the failed call survives.
	res, err := eval(okCtx, doc)
	if err != nil {
		t.Fatalf("descent_budget_recovery first_violation: retry after exhaustion failed: %v", err)
	}
	if got := len(res.([]interface{})); got != visits-1 { // $..* matches every descendant, not root
		t.Fatalf("descent_budget_recovery first_violation: expected %d matches after retry, got %d", visits-1, got)
	}
	// Repeat the tight call: it must fail again identically (no half-update).
	if _, err := eval(tightCtx, doc); !errors.Is(err, jsonpath.ErrRecursiveDescentBudgetExhausted) {
		t.Fatalf("descent_budget_repeatable first_violation: %v", err)
	}
}

// TestRecursiveDescentCyclicInputProvesBudget ensures pathological/cyclic input
// terminates and reports the budget error as its cause, even under the default
// ceiling.
func TestRecursiveDescentCyclicInputProvesBudget(t *testing.T) {
	cycle := arr{}
	cycle = append(cycle, cycle)
	eval, err := jsonpath.New(`$..*`)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := eval(ctx, cycle)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, jsonpath.ErrRecursiveDescentBudgetExhausted) {
			t.Fatalf("descent_cyclic_budget_cause first_violation: expected budget cause, got %v", err)
		}
	case <-timerChan():
		t.Fatal("descent_cyclic_budget_termination first_violation: recursive descent did not terminate on cyclic input")
	}
}

// TestCancellationPreservesCause checks that a cancelled context surfaces the
// context error (not the budget error) and never mutates later calls.
func TestCancellationPreservesCause(t *testing.T) {
	doc := budgetTree(50)
	eval, err := jsonpath.New(`$..*`)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := eval(ctx, doc); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation_cause_preserved first_violation: expected context.Canceled, got %v", err)
	}
	if _, err := eval(context.Background(), doc); err != nil {
		t.Fatalf("cancellation_no_leftover_state first_violation: later call poisoned: %v", err)
	}
}

// TestCorruptInputErrorLayering confirms damaged input keeps its own cause:
// malformed JSON is the caller's problem and must not become a jsonpath error;
// bad queries are parse errors; bad runtime selects are evaluation errors.
func TestCorruptInputErrorLayering(t *testing.T) {
	if _, err := jsonpath.New(`$[`); err == nil {
		t.Fatal("corrupt_query_parse_layer first_violation: expected parse error")
	}
	var broken interface{}
	if err := json.Unmarshal([]byte(`{"a":`), &broken); err == nil {
		t.Fatal("corrupt_json_cause first_violation: json.Unmarshal accepted damaged input")
	} else {
		// The decoder error is preserved untouched.
		var se *json.SyntaxError
		if !errors.As(err, &se) {
			t.Fatalf("corrupt_json_cause first_violation: expected json.SyntaxError, got %T", err)
		}
	}
	doc := obj{"a": 1.}
	got, err := jsonpath.Get(`$.a.b`, doc)
	if err == nil {
		t.Fatalf("corrupt_runtime_eval_layer first_violation: expected evaluation error, got %v", got)
	}
	if got != nil {
		t.Fatalf("corrupt_runtime_eval_layer first_violation: failed call returned partial value %v", got)
	}
}

// TestPlaceholderFailureIsolation proves a failing selector cannot poison the
// placeholder binding reused by a sibling selector within one evaluation, nor
// the shared evaluable reused across calls.
func TestPlaceholderFailureIsolation(t *testing.T) {
	var doc interface{}
	if err := json.Unmarshal([]byte(`{"items":[{"a":1},{"a":2}]}`), &doc); err != nil {
		t.Fatal(err)
	}
	lang := jsonpath.PlaceholderExtension()

	// #0 binds per wildcard match; the second wildcard level provides #1.
	// A branch where #1 is missing must not change the keys seen by branches
	// where it exists.
	eval, err := lang.NewEvaluable(`{#0: $.items[*].a}`)
	if err != nil {
		t.Fatal(err)
	}
	res, err := eval(context.Background(), doc)
	if err != nil {
		t.Fatalf("placeholder_binding_success first_violation: %v", err)
	}
	if want := (obj{"0": 1., "1": 2.}); !reflect.DeepEqual(res, want) {
		t.Fatalf("placeholder_binding_success first_violation:\n  got:  %s\n  want: %s", mustJSON(res), mustJSON(want))
	}

	// Under-bound placeholder: must fail with the placeholder error and no
	// half-constructed object.
	under, err := lang.NewEvaluable(`{#2: $.items[*].a}`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := under(context.Background(), doc); err == nil {
		t.Fatal("placeholder_underbound first_violation: expected placeholder error")
	}
	// Reuse the successful evaluable after the failing one: no pollution.
	res2, err := eval(context.Background(), doc)
	if err != nil {
		t.Fatalf("placeholder_no_cross_evaluable_pollution first_violation: %v", err)
	}
	if !reflect.DeepEqual(res, res2) {
		t.Fatalf("placeholder_no_cross_evaluable_pollution first_violation:\n  before failure: %s\n  after failure : %s",
			mustJSON(res), mustJSON(res2))
	}

	// Interleave success and failure and assert stable results every time.
	bad, err := lang.NewEvaluable(`{#0: $.missing[*]}`)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if _, err := bad(context.Background(), doc); err == nil {
			t.Fatalf("placeholder_failure_stable iteration %d: expected error", i)
		}
		res3, err := eval(context.Background(), doc)
		if err != nil || !reflect.DeepEqual(res3, res) {
			t.Fatalf("placeholder_reuse_stable iteration %d first_violation: %v %s", i, err, mustJSON(res3))
		}
	}
}

// TestEvaluableReuseAcrossFailureAndSuccess interleaves evaluation errors,
// budget aborts and successes on one reusable evaluable and requires identical
// results on full retry.
func TestEvaluableReuseAcrossFailureAndSuccess(t *testing.T) {
	doc := loadCompositionDocument(t)
	eval, err := jsonpath.New(`$..id`)
	if err != nil {
		t.Fatal(err)
	}
	ok, err := eval(context.Background(), doc)
	if err != nil {
		t.Fatal(err)
	}
	zero := context.WithValue(context.Background(), jsonpath.RecursiveDescentBudgetContextKey{}, 0)
	// budget 0 means unlimited; use 1 to force an abort.
	tight := context.WithValue(context.Background(), jsonpath.RecursiveDescentBudgetContextKey{}, 1)
	if _, err := eval(tight, doc); !errors.Is(err, jsonpath.ErrRecursiveDescentBudgetExhausted) {
		t.Fatalf("reuse_mid_failure first_violation: %v", err)
	}
	_ = zero
	again, err := eval(context.Background(), doc)
	if err != nil {
		t.Fatalf("reuse_retry_identical first_violation: %v", err)
	}
	if !reflect.DeepEqual(ok, again) {
		t.Fatalf("reuse_retry_identical first_violation:\n  first: %s\n  retry: %s", mustJSON(ok), mustJSON(again))
	}
}

// TestPropertyGeneratedDocuments generates documents and supported-subset
// queries with a fixed seed and compares production against the reference,
// including values and access positions. Generated selectors stay within the
// reference subset by construction.
func TestPropertyGeneratedDocuments(t *testing.T) {
	seeds := []int64{1, 2, 3}
	for _, seed := range seeds {
		seed := seed
		t.Run("", func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed))
			for iter := 0; iter < 120; iter++ {
				doc, query := genCase(rng)
				label := "property_seed_" + itoa(seed) + "_iter_" + itoa(iter)
				ref := refEvaluate(query, doc)
				if errors.Is(ref.err, errRefRejected) {
					t.Fatalf("%s: generator produced query outside reference subset: %s", label, query)
				}
				eval, perr := jsonpath.New(query)
				if perr != nil {
					t.Fatalf("%s: production rejected reference-supported query %s: %v", label, query, perr)
				}
				got, gerr := eval(context.Background(), doc)
				if ref.err != nil {
					if gerr == nil || gerr.Error() != ref.err.Error() {
						t.Fatalf("%s:\n  query: %s\n  first_violation: error mismatch\n  production: %v\n  reference : %v",
							label, query, gerr, ref.err)
					}
					continue
				}
				if gerr != nil {
					t.Fatalf("%s:\n  query: %s\n  first_violation: unexpected production error: %v", label, query, gerr)
				}
				var w interface{}
				if ref.ambiguous {
					gv, _ := stableValues(got)
					w, _ = stableValues(ref.values())
					if !reflect.DeepEqual(gv, w) {
						t.Fatalf("%s:\n  query: %s\n  first_violation: ambiguous value mismatch\n  production: %s\n  reference : %s",
							label, query, mustJSON(got), mustJSON(ref.values()))
					}
				} else {
					w = ref.values()[0]
					if !reflect.DeepEqual(got, w) {
						t.Fatalf("%s:\n  query: %s\n  first_violation: scalar mismatch\n  production: %s\n  reference : %s",
							label, query, mustJSON(got), mustJSON(w))
					}
				}
			}
		})
	}
}
