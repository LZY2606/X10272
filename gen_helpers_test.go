package jsonpath_test

import (
	"math/rand"
	"strconv"
	"time"
)

func itoa(n int) string { return strconv.Itoa(n) }

func timerChan() <-chan time.Time { return time.After(5 * time.Second) }

// genCase builds a small decoded JSON value and a query within the reference
// subset. It deliberately creates collisions (small key/index alphabets),
// nested arrays/objects and some empty collections so union/slice/descent
// edge cases are hit frequently.
func genCase(rng *rand.Rand) (interface{}, string) {
	keys := []string{"a", "b", "c"}
	var genValue func(depth int) interface{}
	genValue = func(depth int) interface{} {
		if depth == 0 {
			return float64(rng.Intn(4))
		}
		switch rng.Intn(3) {
		case 0:
			n := rng.Intn(4)
			a := make(arr, n)
			for i := range a {
				a[i] = genValue(depth - 1)
			}
			return a
		case 1:
			n := rng.Intn(3)
			m := obj{}
			for i := 0; i < n; i++ {
				m[keys[rng.Intn(len(keys))]] = genValue(depth - 1)
			}
			return m
		default:
			return float64(rng.Intn(3))
		}
	}
	doc := genValue(3)
	if doc == nil {
		doc = obj{}
	}

	// Build a query of 1-3 supported segments.
	segments := 1 + rng.Intn(3)
	q := "$"
	cur := doc
	ambiguous := false
	for i := 0; i < segments; i++ {
		kind := rng.Intn(10)
		switch {
		case kind < 3:
			// direct field or index depending on current node type; empty
			// containers fall back to a wildcard segment.
			wildcard := false
			switch o := cur.(type) {
			case obj:
				ks := make([]string, 0, len(o))
				for k := range o {
					ks = append(ks, k)
				}
				if len(ks) == 0 {
					wildcard = true
					break
				}
				k := ks[rng.Intn(len(ks))]
				q += "." + k
				cur = o[k]
			case arr:
				if len(o) == 0 {
					wildcard = true
					break
				}
				idx := rng.Intn(len(o))
				if rng.Intn(3) == 0 {
					idx = -1 - rng.Intn(len(o))
				}
				q += "[" + strconv.Itoa(idx) + "]"
				p := idx
				if idx < 0 {
					p = len(o) + idx
				}
				if p >= 0 && p < len(o) {
					cur = o[p]
				} else {
					cur = nil
				}
			default:
				// force a type-mismatch case occasionally, then stop
				k := keys[rng.Intn(len(keys))]
				q += "." + k
				cur = nil
			}
			if wildcard {
				if i > 0 {
					q += ".*"
				} else {
					q += "[*]"
				}
				ambiguous = true
				cur = flatten(cur)
			}
		case kind < 6:
			if i > 0 {
				q += ".*"
			} else {
				q += "[*]"
			}
			ambiguous = true
			cur = flatten(cur)
		case kind < 8:
			// slice, only meaningful against arrays
			if a, ok := cur.(arr); ok && len(a) > 0 {
				n := len(a)
				bounds := []int{0, n / 2, n, -1, -n}
				lo := bounds[rng.Intn(len(bounds))]
				hi := bounds[rng.Intn(len(bounds))]
				step := []int{1, 2, -1, -2}[rng.Intn(4)]
				q += "[" + strconv.Itoa(lo) + ":" + strconv.Itoa(hi) + ":" + strconv.Itoa(step) + "]"
			} else {
				q += "[0:2]"
			}
			ambiguous = true
			cur = flatten(cur)
		default:
			// union with small indices/keys
			switch cur.(type) {
			case arr:
				i1 := rng.Intn(4) - 1
				i2 := rng.Intn(4) - 1
				q += "[" + strconv.Itoa(i1) + "," + strconv.Itoa(i2) + "]"
			case obj:
				ks := []string{"a", "b", "c", "missing"}
				q += `["` + ks[rng.Intn(len(ks))] + `","` + ks[rng.Intn(len(ks))] + `"]`
			default:
				q += "[0,1]"
			}
			ambiguous = true
			cur = flatten(cur)
		}
		if ambiguous {
			break
		}
	}
	return doc, q
}

func flatten(v interface{}) interface{} {
	switch o := v.(type) {
	case arr:
		// representative child type cannot be known for all branches; keep v
		return o
	default:
		return v
	}
}
