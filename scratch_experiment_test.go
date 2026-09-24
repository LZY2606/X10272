package jsonpath_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/PaesslerAG/jsonpath"
)

func TestScratchCyclic(t *testing.T) {
	doc := map[string]interface{}{}
	doc["self"] = doc
	eval, err := jsonpath.New("$..x")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	v, err := eval(context.Background(), doc)
	fmt.Println("cyclic default:", v, "err:", err, "elapsed:", time.Since(start))
	ctxb := context.WithValue(context.Background(), jsonpath.RecursiveDescentBudgetContextKey{}, 5)
	v2, err2 := eval(ctxb, doc)
	fmt.Println("cyclic budget5:", v2, "err:", err2)
}

func TestScratchCancel(t *testing.T) {
	var raw interface{}
	json.Unmarshal([]byte(`{"a":{"b":[{"x":1},{"x":2}]}}`), &raw)
	eval, _ := jsonpath.New("$..x")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	v, err := eval(ctx, raw)
	fmt.Println("cancelled result:", v, "err:", err)
}

func TestScratchPlaceholderReuse(t *testing.T) {
	builder := jsonpath.PlaceholderExtension()
	eval, err := builder.NewEvaluable(`{#0: $[*].v}`)
	if err != nil {
		t.Fatal(err)
	}
	doc1 := []interface{}{
		map[string]interface{}{"v": 1},
		map[string]interface{}{"v": 2},
	}
	r1, err := eval(context.Background(), doc1)
	fmt.Println("r1:", r1, err)
	doc2 := []interface{}{
		map[string]interface{}{"v": "a"},
		map[string]interface{}{"missing": true},
		map[string]interface{}{"v": "b"},
	}
	r2, err := eval(context.Background(), doc2)
	fmt.Println("r2:", r2, err)
	r3, err := eval(context.Background(), doc1)
	fmt.Println("r3:", r3, err)
}

func TestScratchMapOrder(t *testing.T) {
	var raw interface{}
	json.Unmarshal([]byte(`{"z":1,"a":2,"m":3,"b":4}`), &raw)
	for i := 0; i < 5; i++ {
		v, _ := jsonpath.Get("$..*", raw)
		fmt.Println("order:", v)
	}
}
