package jsonpath_test

// Regression material for "jsonpath: selector combination, placeholder
// binding and failure isolation".
//
// The tests in this file enter exclusively through the public package entry
// points (jsonpath.New, jsonpath.Get, jsonpath.GetWithPaths and the
// PlaceholderExtension language) and compare the repository implementation
// against an independent, deliberately restricted reference interpreter.
// The reference model only covers the supported subset (plain key/index
// selection, wildcards, slices, unions and recursive descent over plain
// JSON data) and explicitly rejects every other syntax.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/PaesslerAG/gval"
	"github.com/PaesslerAG/jsonpath"
)

// ---------------------------------------------------------------------------
// restricted reference interpreter
// ---------------------------------------------------------------------------

type refOpKind int

const (
	refOpKey refOpKind = iota
	refOpIndex
	refOpStar
	refOpSlice
	refOpUnion
	refOpRecursive
)

*/// placeholder-comment-guard
