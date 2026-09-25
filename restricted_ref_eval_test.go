package jsonpath_test

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// refHit is one match: value and canonical access position.
type refHit struct {
	path string
	val  interface{}
}

type refOutcome struct {
	hits      []refHit
	err       error
	ambiguous bool
}

func refEvaluate(query string, root interface{}) refOutcome {
	segs, err := refParse(query)
	if err != nil {
		return refOutcome{err: err}
	}
	hits := []refHit{{path: "$", val: root}}
	amb := false
	for _, seg := range segs {
		var next []refHit
		if seg.kind == 'r' {
			amb = true
			for _, h := range hits {
				desc, derr := refApplyDescent(h, seg)
				if derr != nil {
					return refOutcome{err: derr}
				}
				next = append(next, desc...)
			}
		} else {
			anyAmbiguous := seg.kind == 's' || seg.kind == 'u' || seg.kind == 'c'
			for _, h := range hits {
				hs, herr := refApplySegment(h, seg)
				if herr != nil {
					return refOutcome{err: herr}
				}
				if anyAmbiguous {
					amb = true
				}
				next = append(next, hs...)
			}
		}
		hits = next
	}
	return refOutcome{hits: hits, ambiguous: amb}
}

func refApplySegment(h refHit, seg refSegment) ([]refHit, error) {
	switch seg.kind {
	case 'd':
		val, perr := refSelectOne(h.val, seg.key)
		if perr != nil {
			return nil, perr
		}
		return []refHit{{path: refJoin(h.path, seg.key), val: val}}, nil
	case 's':
		var out []refHit
		refChildren(h.val, func(k string, v interface{}) {
			out = append(out, refHit{path: refJoin(h.path, k), val: v})
		})
		return out, nil
	case 'u':
		var out []refHit
		for _, k := range seg.keys {
			val, perr := refSelectOne(h.val, k)
			if perr != nil {
				// missing object keys / type mismatches drop the branch
				continue
			}
			out = append(out, refHit{path: refJoin(h.path, k), val: val})
		}
		return out, nil
	case 'c':
		return refApplySlice(h, seg.lo, seg.hi, seg.st), nil
	}
	return nil, fmt.Errorf("%w: unknown segment kind", errRefRejected)
}

func refApplyDescent(h refHit, seg refSegment) ([]refHit, error) {
	var out []refHit
	var walk func(prefix string, v interface{}) error
	walk = func(prefix string, v interface{}) error {
		hs, err := refAfterDescent(prefix, v, seg)
		if err != nil {
			return err
		}
		out = append(out, hs...)
		var walkErr error
		refChildren(v, func(k string, child interface{}) {
			if walkErr != nil {
				return
			}
			walkErr = walk(refJoin(prefix, k), child)
		})
		return walkErr
	}
	err := walk(h.path, h.val)
	return out, err
}

func refAfterDescent(prefix string, v interface{}, seg refSegment) ([]refHit, error) {
	tmp := refHit{path: prefix, val: v}
	switch seg.after {
	case 'd':
		val, err := refSelectOne(v, seg.akey)
		if err != nil {
			return nil, err
		}
		return []refHit{{path: refJoin(prefix, seg.akey), val: val}}, nil
	case 's':
		var out []refHit
		refChildren(v, func(k string, cv interface{}) {
			out = append(out, refHit{path: refJoin(prefix, k), val: cv})
		})
		return out, nil
	case 'u':
		var out []refHit
		for _, k := range seg.akeys {
			val, err := refSelectOne(v, k)
			if err != nil {
				continue
			}
			out = append(out, refHit{path: refJoin(prefix, k), val: val})
		}
		return out, nil
	case 'c':
		return refApplySlice(tmp, seg.alo, seg.ahi, seg.ast), nil
	default:
		return nil, fmt.Errorf("%w: recursive descent without following selector", errRefRejected)
	}
}

// refSelectOne mirrors the production direct-select semantics, including its
// quirks and error wording, for plain decoded JSON values.
func refSelectOne(v interface{}, key string) (interface{}, error) {
	if i, err := strconv.Atoi(key); err == nil {
		switch o := v.(type) {
		case []interface{}:
			p := i
			if i < 0 {
				p = len(o) + i
			}
			if p < 0 || p >= len(o) {
				return nil, nil
			}
			return o[p], nil
		case map[string]interface{}:
			if r, ok := o[strconv.Itoa(i)]; ok {
				return r, nil
			}
			return nil, fmt.Errorf("unknown key %d", i)
		default:
			return nil, fmt.Errorf("unsupported value type %s for select, expected map[string]interface{}, []interface{} or Array", refTypeName(v))
		}
	}
	switch o := v.(type) {
	case map[string]interface{}:
		if r, ok := o[key]; ok {
			return r, nil
		}
		return nil, fmt.Errorf("unknown key %s", key)
	case []interface{}:
		return nil, fmt.Errorf("could not select value, invalid key: expected number but got %s (string)", key)
	default:
		return nil, fmt.Errorf("unsupported value type %s for select, expected map[string]interface{}, []interface{} or Array", refTypeName(v))
	}
}

func refTypeName(v interface{}) string {
	switch v.(type) {
	case nil:
		return "<nil>"
	case string:
		return "string"
	case float64:
		return "float64"
	case bool:
		return "bool"
	case []interface{}:
		return "[]interface {}"
	case map[string]interface{}:
		return "map[string]interface {}"
	default:
		return fmt.Sprintf("%T", v)
	}
}

// refChildren visits arrays by index and objects by sorted key, matching the
// repository's reproducible traversal order.
func refChildren(v interface{}, visit func(k string, cv interface{})) {
	switch o := v.(type) {
	case []interface{}:
		for i, e := range o {
			visit(strconv.Itoa(i), e)
		}
	case map[string]interface{}:
		keys := make([]string, 0, len(o))
		for k := range o {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			visit(k, o[k])
		}
	}
}

func refApplySlice(h refHit, lop, hip, stp *int) []refHit {
	o, ok := h.val.([]interface{})
	if !ok {
		return nil
	}
	n := len(o)
	lo, hi, st := 0, n, 1
	if lop != nil {
		lo = *lop
	}
	if hip != nil {
		hi = *hip
	}
	if stp != nil {
		st = *stp
	}
	if st == 0 {
		st = 1
	}
	lo = refNegmax(lo, n)
	hi = refNegmax(hi, n)
	var out []refHit
	if lo > hi {
		return out
	}
	if st > 0 {
		for i := lo; i < hi; i += st {
			out = append(out, refHit{path: refJoin(h.path, strconv.Itoa(i)), val: o[i]})
		}
	} else {
		for i := hi - 1; i >= lo; i += st {
			out = append(out, refHit{path: refJoin(h.path, strconv.Itoa(i)), val: o[i]})
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

func refJoin(prefix, key string) string {
	return fmt.Sprintf("%s[%q]", prefix, key)
}

func (o refOutcome) values() []interface{} {
	vs := make([]interface{}, 0, len(o.hits))
	for _, h := range o.hits {
		vs = append(vs, h.val)
	}
	return vs
}

func (o refOutcome) pathMap() map[string]interface{} {
	m := map[string]interface{}{}
	for _, h := range o.hits {
		m[h.path] = h.val
	}
	return m
}

func refPrefixes(query string) ([]string, error) {
	segs, err := refParse(query)
	if err != nil {
		return nil, err
	}
	out := []string{"$"}
	for i := range segs {
		out = append(out, refRender(segs[:i+1]))
	}
	return out, nil
}

func refRender(segs []refSegment) string {
	var sb strings.Builder
	sb.WriteByte('$')
	for _, seg := range segs {
		if seg.kind == 'r' {
			sb.WriteString("..")
			switch seg.after {
			case 'd':
				sb.WriteString(seg.akey)
			case 's':
				sb.WriteString("*")
			case 'u':
				sb.WriteByte('[')
				for i, k := range seg.akeys {
					if i > 0 {
						sb.WriteByte(',')
					}
					sb.WriteString(refRenderKey(k))
				}
				sb.WriteByte(']')
			case 'c':
				sb.WriteString(refRenderSlice(seg.alo, seg.ahi, seg.ast))
			}
			continue
		}
		switch seg.kind {
		case 'd':
			sb.WriteByte('.')
			sb.WriteString(seg.key)
		case 's':
			sb.WriteString(".*")
		case 'u':
			sb.WriteByte('[')
			for i, k := range seg.keys {
				if i > 0 {
					sb.WriteByte(',')
				}
				sb.WriteString(refRenderKey(k))
			}
			sb.WriteByte(']')
		case 'c':
			sb.WriteString(refRenderSlice(seg.lo, seg.hi, seg.st))
		}
	}
	return sb.String()
}

func refRenderKey(k string) string {
	if _, err := strconv.Atoi(k); err == nil {
		return k
	}
	return strconv.Quote(k)
}

func refRenderSlice(lo, hi, st *int) string {
	b := func(p *int) string {
		if p == nil {
			return ""
		}
		return strconv.Itoa(*p)
	}
	s := "[" + b(lo) + ":" + b(hi)
	if st != nil {
		s += ":" + b(st)
	}
	return s + "]"
}
