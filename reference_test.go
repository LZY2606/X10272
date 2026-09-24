package jsonpath_test

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// This file implements an independent, restricted reference interpreter for
// the JSONPath subset supported by this repository: the root $, child access
// by key or (negative) index, wildcards, unions, slices and recursive
// descent. Anything outside that subset (filters, scripts, placeholders, the
// current element, single quoted keys, recursive descent combined with
// slices, ...) is explicitly rejected. The composition tests hand the paths
// parsed by the repository parser to this reference model and compare both
// the selected values and the accessed positions.

type refKind int

const (
	refKey refKind = iota
	refIndex
	refWildcard
	refUnion
	refSlice
)

// refStep is one selector of a reference path.
type refStep struct {
	recursive bool
	kind      refKind
	key       string
	index     int
	union     []refStep
	min       int
	max       int
	step      int
}

// refMatch is one matched value together with its canonical access path.
type refMatch struct {
	path  string
	value interface{}
}

func mustJSON(t *testing.T, data string) interface{} {
	t.Helper()
	var v interface{}
	if err := json.Unmarshal([]byte(data), &v); err != nil {
		t.Fatalf("could not parse json input %s: %v", data, err)
	}
	return v
}

func isRefIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isRefIdent(c byte) bool {
	return isRefIdentStart(c) || (c >= '0' && c <= '9')
}

func parseRefPath(path string) ([]refStep, error) {
	if !strings.HasPrefix(path, "$") {
		return nil, fmt.Errorf("reference model: path %q must start with $", path)
	}
	rest := path[1:]
	steps := []refStep{}
	for len(rest) > 0 {
		var step refStep
		var err error
		switch {
		case strings.HasPrefix(rest, ".."):
			step.recursive = true
			rest, err = parseRefSelector(rest[2:], &step)
			if err == nil && step.kind == refSlice {
				err = fmt.Errorf("reference model: recursive descent cannot be combined with slice")
			}
		case strings.HasPrefix(rest, "."):
			rest, err = parseRefSelector(rest[1:], &step)
		case strings.HasPrefix(rest, "["):
			rest, err = parseRefBracket(rest, &step)
		default:
			err = fmt.Errorf("reference model: unsupported syntax near %q", rest)
		}
		if err != nil {
			return nil, err
		}
		steps = append(steps, step)
	}
	return steps, nil
}

func parseRefSelector(rest string, step *refStep) (string, error) {
	switch {
	case rest == "":
		return "", fmt.Errorf("reference model: unexpected end of path")
	case strings.HasPrefix(rest, "*"):
		step.kind = refWildcard
		return rest[1:], nil
	case strings.HasPrefix(rest, "["):
		return parseRefBracket(rest, step)
	case isRefIdentStart(rest[0]):
		i := 0
		for i < len(rest) && isRefIdent(rest[i]) {
			i++
		}
		step.kind = refKey
		step.key = rest[:i]
		return rest[i:], nil
	default:
		return "", fmt.Errorf("reference model: unsupported syntax near %q", rest)
	}
}

func parseRefBracket(rest string, step *refStep) (string, error) {
	quote := false
	end := -1
	for i := 1; i < len(rest); i++ {
		switch {
		case quote:
			if rest[i] == '"' {
				quote = false
			}
		case rest[i] == '"':
			quote = true
		case rest[i] == ']':
			end = i
		}
		if end >= 0 {
			break
		}
	}
	if end < 0 {
		return "", fmt.Errorf("reference model: unclosed bracket in %q", rest)
	}
	content := strings.TrimSpace(rest[1:end])
	remaining := rest[end+1:]
	switch {
	case content == "*":
		step.kind = refWildcard
	case !strings.HasPrefix(content, `"`) && strings.Contains(content, ":"):
		parts := strings.Split(content, ":")
		if len(parts) > 3 {
			return "", fmt.Errorf("reference model: range query has at least the parameter [min:max:step]")
		}
		bounds := []int{0, math.MaxInt32, 1}
		for i, part := range parts {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			n, err := strconv.Atoi(part)
			if err != nil {
				return "", fmt.Errorf("reference model: invalid slice bound %q", part)
			}
			bounds[i] = n
		}
		step.kind = refSlice
		step.min, step.max, step.step = bounds[0], bounds[1], bounds[2]
	default:
		items, err := refSplitUnion(content)
		if err != nil {
			return "", err
		}
		if len(items) == 1 {
			single, err := refKeyOrIndex(items[0])
			if err != nil {
				return "", err
			}
			*step = single
			break
		}
		step.kind = refUnion
		for _, item := range items {
			member, err := refKeyOrIndex(item)
			if err != nil {
				return "", err
			}
			step.union = append(step.union, member)
		}
	}
	return remaining, nil
}

func refSplitUnion(content string) ([]string, error) {
	parts := []string{}
	quote := false
	start := 0
	for i := 0; i < len(content); i++ {
		switch {
		case quote:
			if content[i] == '"' {
				quote = false
			}
		case content[i] == '"':
			quote = true
		case content[i] == ',':
			parts = append(parts, strings.TrimSpace(content[start:i]))
			start = i + 1
		}
	}
	parts = append(parts, strings.TrimSpace(content[start:]))
	for _, part := range parts {
		if part == "" {
			return nil, fmt.Errorf("reference model: empty union member in %q", content)
		}
	}
	return parts, nil
}

func refKeyOrIndex(item string) (refStep, error) {
	if strings.HasPrefix(item, `"`) {
		key, err := strconv.Unquote(item)
		if err != nil {
			return refStep{}, fmt.Errorf("reference model: invalid string key %s", item)
		}
		return refStep{kind: refKey, key: key}, nil
	}
	index, err := strconv.Atoi(item)
	if err != nil {
		return refStep{}, fmt.Errorf("reference model: unsupported bracket member %q", item)
	}
	return refStep{kind: refIndex, index: index}, nil
}

func refIsAmbiguous(steps []refStep) bool {
	for _, step := range steps {
		if step.recursive || step.kind == refWildcard || step.kind == refUnion || step.kind == refSlice {
			return true
		}
	}
	return false
}

// evalRefPath evaluates a reference path. Once a path contains an ambiguous
// selector, failing selections select nothing, mirroring the repository
// semantics; fully plain paths report the first error.
func evalRefPath(steps []refStep, doc interface{}) ([]refMatch, error) {
	ambiguous := refIsAmbiguous(steps)
	matches := []refMatch{{path: "$", value: doc}}
	for _, step := range steps {
		targets := matches
		if step.recursive {
			targets = []refMatch{}
			for _, m := range matches {
				targets = append(targets, refDescendants(m)...)
			}
		}
		next := []refMatch{}
		for _, target := range targets {
			got, err := applyRefStep(step, target)
			if err != nil {
				if ambiguous {
					continue
				}
				return nil, err
			}
			next = append(next, got...)
		}
		matches = next
	}
	return matches, nil
}

func applyRefStep(step refStep, target refMatch) ([]refMatch, error) {
	switch step.kind {
	case refKey:
		return refSelectKey(target, step.key)
	case refIndex:
		return refSelectIndex(target, step.index)
	case refWildcard:
		return refWildcardMatches(target), nil
	case refUnion:
		out := []refMatch{}
		for _, member := range step.union {
			var got []refMatch
			var err error
			if member.kind == refIndex {
				got, err = refSelectIndex(target, member.index)
			} else {
				got, err = refSelectKey(target, member.key)
			}
			if err != nil {
				continue
			}
			out = append(out, got...)
		}
		return out, nil
	case refSlice:
		return refSliceMatches(target, step), nil
	default:
		return nil, fmt.Errorf("reference model: unknown selector kind %d", step.kind)
	}
}

func refSelectKey(target refMatch, key string) ([]refMatch, error) {
	switch o := target.value.(type) {
	case map[string]interface{}:
		v, ok := o[key]
		if !ok {
			return nil, fmt.Errorf("unknown key %s", key)
		}
		return []refMatch{{refJoin(target.path, key), v}}, nil
	case []interface{}:
		return nil, fmt.Errorf("could not select value, invalid key: expected number but got %s (string)", key)
	default:
		return nil, fmt.Errorf("unsupported value type %T for select, expected map[string]interface{}, []interface{} or Array", o)
	}
}

func refSelectIndex(target refMatch, index int) ([]refMatch, error) {
	jkey := strconv.Itoa(index)
	switch o := target.value.(type) {
	case []interface{}:
		p := index
		if p < 0 {
			p = len(o) + p
		}
		if p < 0 || p >= len(o) {
			return []refMatch{{refJoin(target.path, jkey), nil}}, nil
		}
		return []refMatch{{refJoin(target.path, jkey), o[p]}}, nil
	case map[string]interface{}:
		v, ok := o[jkey]
		if !ok {
			return nil, fmt.Errorf("unknown key %s", jkey)
		}
		return []refMatch{{refJoin(target.path, jkey), v}}, nil
	default:
		return nil, fmt.Errorf("unsupported value type %T for select, expected map[string]interface{}, []interface{} or Array", o)
	}
}

func refWildcardMatches(target refMatch) []refMatch {
	out := []refMatch{}
	switch o := target.value.(type) {
	case map[string]interface{}:
		for _, k := range refSortedKeys(o) {
			out = append(out, refMatch{refJoin(target.path, k), o[k]})
		}
	case []interface{}:
		for i, v := range o {
			out = append(out, refMatch{refJoin(target.path, strconv.Itoa(i)), v})
		}
	}
	return out
}

func refSliceMatches(target refMatch, step refStep) []refMatch {
	o, ok := target.value.([]interface{})
	if !ok {
		return nil
	}
	n := len(o)
	min := refNegmax(step.min, n)
	max := refNegmax(step.max, n)
	s := step.step
	if s == 0 {
		s = 1
	}
	if min > max {
		return nil
	}
	out := []refMatch{}
	if s > 0 {
		for i := min; i < max; i += s {
			out = append(out, refMatch{refJoin(target.path, strconv.Itoa(i)), o[i]})
		}
	} else {
		for i := max - 1; i >= min; i += s {
			out = append(out, refMatch{refJoin(target.path, strconv.Itoa(i)), o[i]})
		}
	}
	return out
}

func refNegmax(n, max int) int {
	if n < 0 {
		n = max + n
		if n < 0 {
			n = 0
		}
	} else if n > max {
		return max
	}
	return n
}

// refDescendants enumerates a node and all its descendants in pre-order,
// visiting object keys in sorted order so traversal is reproducible.
func refDescendants(m refMatch) []refMatch {
	out := []refMatch{m}
	switch o := m.value.(type) {
	case map[string]interface{}:
		for _, k := range refSortedKeys(o) {
			out = append(out, refDescendants(refMatch{refJoin(m.path, k), o[k]})...)
		}
	case []interface{}:
		for i, v := range o {
			out = append(out, refDescendants(refMatch{refJoin(m.path, strconv.Itoa(i)), v})...)
		}
	}
	return out
}

func refSortedKeys(o map[string]interface{}) []string {
	keys := make([]string, 0, len(o))
	for k := range o {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func refJoin(path, segment string) string {
	return path + "[" + strconv.Quote(segment) + "]"
}

// refExpectValue assembles the value a Get call must return for the matches.
func refExpectValue(steps []refStep, matches []refMatch) interface{} {
	if refIsAmbiguous(steps) {
		values := []interface{}{}
		for _, m := range matches {
			values = append(values, m.value)
		}
		return values
	}
	if len(matches) == 0 {
		return nil
	}
	return matches[0].value
}

// refExpectPaths assembles the access position map a GetWithPaths call must
// return for the matches.
func refExpectPaths(matches []refMatch) map[string]interface{} {
	out := map[string]interface{}{}
	for _, m := range matches {
		out[m.path] = m.value
	}
	return out
}
