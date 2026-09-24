package jsonpath

import "context"

// SegmentKind identifies one parsed JSONPath segment in the restricted
// structural representation that is emitted by the production parser. It is
// exported for testing purposes only.
type SegmentKind string

const (
	// SegmentCurrent is the leading "@" element.
	SegmentCurrent SegmentKind = "current"
	// SegmentDirect is a single plain selector ".x" or "[x]".
	SegmentDirect SegmentKind = "direct"
	// SegmentStar is an object/array wildcard "*" or "[*]".
	SegmentStar SegmentKind = "star"
	// SegmentMulti is a union "[a, b]".
	SegmentMulti SegmentKind = "multi"
	// SegmentSlice is a range "[min:max:step]".
	SegmentSlice SegmentKind = "slice"
	// SegmentRecursive is a recursive descent "..".
	SegmentRecursive SegmentKind = "recursive"
	// SegmentFilter is a filter expression "[?expr]".
	SegmentFilter SegmentKind = "filter"
	// SegmentScript is a script selector "(expr)".
	SegmentScript SegmentKind = "script"
)

// SliceBounds holds the literal bounds of a slice segment. A false
// *Constant flag marks an endpoint that is not a compile-time constant and
// therefore lies outside the restricted reference subset.
type SliceBounds struct {
	Min           interface{}
	MinConstant   bool
	Max           interface{}
	MaxConstant   bool
	Step          interface{}
	StepConstant bool
}

// ParsedSegment is one segment of a JSONPath as parsed by the production
// parser. It is exported for testing purposes and intentionally carries only
// structural information; the production evaluables remain the only
// implementation of the evaluation semantics.
type ParsedSegment struct {
	Kind  SegmentKind
	Key   interface{}
	Keys  []interface{}
	Slice *SliceBounds
}

// ParsedSegmentsContextKey is exported for testing purposes. Passing a
// context with *[]ParsedSegment to NewEvaluableWithContext makes the
// production parser append the structural representation of every JSONPath
// it parses (including paths embedded in placeholder objects) to that slice.
// The reference test interpreter consumes this representation, so parsing
// stays entirely on the production path and is never duplicated in tests.
type ParsedSegmentsContextKey struct{}
