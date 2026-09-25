package jsonpath_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/PaesslerAG/jsonpath"
)

// This file is the regression material for
// "jsonpath: selector composition, placeholder binding and failure isolation".
// Every case enters the repository only through its public surface
// (jsonpath.New / Evaluable / GetWithPaths / PlaceholderExtension), parses the
// query with the real parser, and compares values AND canonical access
// positions against restricted_ref_test.go, an independent interpreter limited
// to the supported subset.

func loadCompositionDocument(t *testing.T) interface{} {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "selector_composition", "document.json"))
	if err != nil {
		t.Fatalf("composition_cannot_read_fixture: %v", err)
	}
	var doc interface{}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("composition_fixture_is_not_json at byte 0: %v", err)
	}
	return doc
}

func mustJSON(v interface{}) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// stableValues canonicalises an observable result for order-independent
// comparison: successful ambiguous results are []interface{}, plain results
// are returned as-is.
func stableValues(got interface{}) (interface{}, bool) {
	if g, ok := got.([]interface{}); ok {
		out := make([]string, 0, len(g))
		for _, e := range g {
			out = append(out, mustJSON(e))
		}
		sort.Strings(out)
		return out, true
	}
	return got, false
}

func sortedPathMap(m map[string]interface{}) [][2]string {
	out := make([][2]string, 0, len(m))
	for k, v := range m {
		out = append(out, [2]string{k, mustJSON(v)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	return out
}

// expectRefError compares a production error with the reference outcome.
// Reference parse rejections are a different error layer and are handled by
// the caller; here both sides must be evaluation errors with the same message.
func expectRefError(t *testing.T, label, query string, got error, ref refOutcome) bool {
	t.Helper()
	if got == nil {
		t.Errorf("%s:\n  query: %s\n  first_violation: production evaluated successfully (%s)\n  reference error: %v",
			label, query, mustJSON(nil), ref.err)
		return false
	}
	if got.Error() != ref.err.Error() {
		t.Errorf("%s:\n  query: %s\n  first_violation: evaluation error mismatch\n  production: %q\n  reference : %q",
			label, query, got.Error(), ref.err.Error())
		return false
	}
	return true
}

// runAgainstReference compares one (query, document) pair between the
// production entry point and the restricted reference, for both values and
// access positions (GetWithPaths).
func runAgainstReference(t *testing.T, label, query string, doc interface{}) {
	t.Helper()
	ref := refEvaluate(query, doc)
	eval, err := jsonpath.New(query)
	if errors.Is(ref.err, errRefRejected) {
		t.Fatalf("%s:\n  query: %s\n  reference setup error: reference must not reject a query used in this group: %v",
			label, query, ref.err)
	}
	if err != nil {
		t.Fatalf("%s:\n  query: %s\n  first_violation: production parse error inside supported subset: %v", label, query, err)
	}

	got, gerr := eval(context.Background(), doc)
	if ref.err != nil {
		expectRefError(t, label, query, gerr, ref)
		return
	}
	if gerr != nil {
		t.Fatalf("%s:\n  query: %s\n  first_violation: unexpected production evaluation error: %v", label, query, gerr)
	}

	want := ref.values()
	if !ref.ambiguous {
		if len(want) != 1 {
			t.Fatalf("%s:\n  query: %s\n  first_violation: reference invariant broken, non-ambiguous query yielded %d matches",
				label, query, len(want))
		}
		if !reflect.DeepEqual(got, want[0]) {
			t.Errorf("%s:\n  query: %s\n  first_violation: scalar value mismatch\n  production: %s\n  reference : %s",
				label, query, mustJSON(got), mustJSON(want[0]))
		}
	} else {
		g, _ := stableValues(got)
		w, _ := stableValues(want)
		if !reflect.DeepEqual(g, w) {
			t.Errorf("%s:\n  query: %s\n  first_violation: ambiguous values mismatch\n  production: %s\n  reference : %s",
				label, query, mustJSON(got), mustJSON(want))
		}
	}

	// Access positions: compare the GetWithPaths map with reference paths.
	pv, perr := jsonpath.GetWithPaths(query, doc)
	if perr != nil {
		t.Fatalf("%s:\n  query: %s\n  first_violation: GetWithPaths error: %v", label, query, perr)
	}
	gm, ok := pv.(map[string]interface{})
	if !ok {
		t.Fatalf("%s:\n  query: %s\n  first_violation: GetWithPaths did not return map, got %T", label, query, pv)
	}
	wm := ref.pathMap()
	if gs, ws := sortedPathMap(gm), sortedPathMap(wm); !reflect.DeepEqual(gs, ws) {
		t.Errorf("%s:\n  query: %s\n  first_violation: access positions mismatch\n  production: %v\n  reference : %v",
			label, query, gs, ws)
	}
}

// compositionCase is one fixed counter-example with the invariant it
// documents as its name, so a failing test name already states the contract.
type compositionCase struct {
	name  string
	query string
}

func compositionCases() []compositionCase {
	return []compositionCase{
		// Array negative index and slices, including exact-end coverage that
		// catches off-by-one mutations.
		{"selector_composition_negative_index_last", "$.nums[-1]"},
		{"selector_composition_negative_index_first", "$.nums[-5]"},
		{"selector_composition_slice_exact_endpoints", "$.nums[0:5]"},
		{"selector_composition_slice_half_open", "$.nums[1:4]"},
		{"selector_composition_slice_tail", "$.nums[4:]"},
		{"selector_composition_slice_head", "$.nums[:1]"},
		{"selector_composition_slice_negative_tail", "$.nums[-2:]"},
		{"selector_composition_slice_negative_head", "$.nums[:-1]"},
		{"selector_composition_slice_step", "$.nums[::2]"},
		{"selector_composition_slice_reverse", "$.nums[::-1]"},
		{"selector_composition_slice_negative_window", "$.nums[-4:-1]"},
		{"selector_composition_slice_empty_window", "$.nums[0:0]"},
		{"selector_composition_slice_out_of_bounds", "$.nums[7:10]"},
		// Object wildcard with reproducible traversal order.
		{"selector_composition_object_wildcard", "$.store.*"},
		{"selector_composition_bracket_wildcard_then_field", "$.store.books[*].id"},
		// Recursive descent in multiple shapes.
		{"selector_composition_descent_field", "$..id"},
		{"selector_composition_descent_wildcard", "$.deep..*"},
		{"selector_composition_descent_union", "$..[id,count]"},
		{"selector_composition_descent_then_wildcard", "$.x..*"},
		// Unions, including the negative/positive out-of-bounds quirk.
		{"selector_composition_union_array", "$.nums[1,3]"},
		{"selector_composition_union_with_oob_positive", "$.nums[1,9]"},
		{"selector_composition_union_with_oob_negative", "$.nums[1,-9]"},
		{"selector_composition_union_then_slice", "$.list[*].v"},
		// Deep empty collections: descent and wildcards over [], {} and nested
		// empties produce zero matches, not errors.
		{"selector_composition_deep_empty_descent", "$.empties..missing"},
		{"selector_composition_deep_empty_wildcard", "$.empties[*][*]"},
		{"selector_composition_descent_inside_empty_object", "$.deep.l1.l2.l3.nothing..x"},
		// Type mismatches: evaluation error layer, repeatable message.
		{"selector_composition_type_mismatch_field_on_array", "$.nums.missing"},
		{"selector_composition_type_mismatch_index_on_scalar", "$.store.count[0]"},
		{"selector_composition_unknown_object_key", "$.store.missing"},
	}
}

func TestSelectorComposition(t *testing.T) {
	doc := loadCompositionDocument(t)
	for _, tc := range compositionCases() {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			runAgainstReference(t, "TestSelectorComposition", tc.query, doc)
		})
	}
}

// TestSelectorCompositionByPrefix exercises the composition boundary: every
// selector is checked both standing alone (the prefix path through the public
// parser/evaluator) and inside the full path. The parser output is never
// fabricated; each prefix is independently parsed through jsonpath.New.
func TestSelectorCompositionByPrefix(t *testing.T) {
	doc := loadCompositionDocument(t)
	full := []string{
		"$.store.books[*].tags[0]",
		"$.nums[0:5:2]",
		"$..id",
		"$.store..books[*].meta",
		"$.list[0:3][*].v",
	}
	for _, query := range full {
		prefixes, err := refPrefixes(query)
		if err != nil {
			t.Fatalf("selector_level_reference_rejects_own_case: %s: %v", query, err)
		}
		for i, prefix := range prefixes {
			prefix := prefix
			t.Run(fmt.Sprintf("%s/prefix_%d_%s", sanitize(query), i, sanitize(prefix)), func(t *testing.T) {
				runAgainstReference(t, "TestSelectorCompositionByPrefix", prefix, doc)
			})
		}
	}
}

func sanitize(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		b := s[i]
		switch {
		case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
			out = append(out, b)
		default:
			out = append(out, '_')
		}
	}
	return string(out)
}
