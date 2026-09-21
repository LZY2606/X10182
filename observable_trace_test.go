package gval_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/PaesslerAG/gval"
)

// masterExpression combines:
//   - a camouflaged token (the closing ']' is rewound and reused as an operator),
//   - binary operators of different precedence ('*' 150, '+' 120, '==' 40,
//     '&&' 21, '||' 20),
//   - short circuiting booleans ('&&' and '||'),
//   - a dynamic subscript ('[idx]' whose key is itself an evaluable path), and
//   - the custom Selector below (pathRecorder implements gval.Selector).
const masterExpression = `2 * 3 + 4 == 10 && probe.items[idx] > 5 || probe.secret == 99`

// pathVisit records one SelectGVal call together with the context state
// observed at that boundary.
type pathVisit struct {
	key    string
	ctxErr string
}

// pathRecorder is a controllable, recording gval.Selector. Every path segment
// that reaches it is appended to visits, together with the current context
// error. Fields below "private" are reported as invisible, and an optional
// cancel hook lets a visit cancel the context mid-evaluation.
type pathRecorder struct {
	visits []pathVisit
	data   map[string]interface{}
	cancel context.CancelFunc
}

var _ gval.Selector = (*pathRecorder)(nil)

func (r *pathRecorder) SelectGVal(ctx context.Context, key string) (interface{}, error) {
	ce := ""
	if ctx.Err() != nil {
		ce = ctx.Err().Error()
	}
	r.visits = append(r.visits, pathVisit{key: key, ctxErr: ce})
	if r.cancel != nil && key == "step" {
		r.cancel()
	}
	if key == "private" {
		return nil, fmt.Errorf("invisible field %q", key)
	}
	return r.data[key], nil
}

func keys(visits []pathVisit) []string {
	out := make([]string, 0, len(visits))
	for _, v := range visits {
		out = append(out, v.key)
	}
	return out
}

func masterRoot() (*pathRecorder, *pathRecorder) {
	child := &pathRecorder{data: map[string]interface{}{
		"items":  []interface{}{3.0, 7.0, 9.0},
		"secret": 99.0,
	}}
	root := &pathRecorder{data: map[string]interface{}{"probe": child, "idx": 1.0}}
	return root, child
}

// Example_observableTrace walks masterExpression end to end with the recording
// selector. The printed key sequence shows the root parameter ("probe"), the
// dynamic index resolved against that same root ("idx"), the nested static
// segment ("items"), and the omission of "secret" caused by the "||" short
// circuit.
func Example_observableTrace() {
	root, child := masterRoot()

	value, err := gval.Full().Evaluate(masterExpression, root)
	fmt.Println("value:", value)
	fmt.Println("error:", err)
	fmt.Println("root visits:", keys(root.visits))
	fmt.Println("probe visits:", keys(child.visits))
	// Output:
	// value: true
	// error: <nil>
	// root visits: [probe idx]
	// probe visits: [items]
}

func TestObservableMasterTrace(t *testing.T) {
	root, child := masterRoot()

	value, err := gval.Full().Evaluate(masterExpression, root)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if value != true {
		t.Fatalf("value = %v, want true", value)
	}
	if got := keys(root.visits); strings.Join(got, ",") != "probe,idx" {
		t.Fatalf("root visits = %v, want [probe idx]", got)
	}
	if got := keys(child.visits); strings.Join(got, ",") != "items" {
		t.Fatalf("probe visits = %v, want [items]", got)
	}
}

// TestObservableShortCircuit proves the right operand of a decided boolean is
// compiled but never evaluated: the selector records no visit at all.
func TestObservableShortCircuit(t *testing.T) {
	lang := gval.Full()

	cases := []struct {
		name string
		expr string
		root map[string]interface{}
		want interface{}
	}{
		{"and false", `false && probe.secret == 99`,
			map[string]interface{}{"probe": &pathRecorder{data: map[string]interface{}{"secret": 99.0}}}, false},
		{"or true", `true || probe.secret == 99`,
			map[string]interface{}{"probe": &pathRecorder{data: map[string]interface{}{"secret": 99.0}}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := tc.root["probe"].(*pathRecorder)
			got, err := lang.Evaluate(tc.expr, tc.root)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("value = %v, want %v", got, tc.want)
			}
			if len(rec.visits) != 0 {
				t.Fatalf("short-circuited side visited %v, want no visits", keys(rec.visits))
			}
		})
	}
}

// TestObservableDynamicIndexRoot proves a dynamic subscript is evaluated
// against the original root parameter, not the current segment: the same key
// "idx" exists on both root (2) and the probe selector (0), yet the selected
// array element is the root-indexed one (9).
func TestObservableDynamicIndexRoot(t *testing.T) {
	child := &pathRecorder{data: map[string]interface{}{
		"items": []interface{}{3.0, 7.0, 9.0},
		"idx":   0.0,
	}}
	root := &pathRecorder{data: map[string]interface{}{"probe": child, "idx": 2.0}}

	value, err := gval.Full().Evaluate(`probe.items[idx]`, root)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if value != 9.0 {
		t.Fatalf("value = %v, want 9 (root idx), proving the dynamic key uses the root object", value)
	}
	if got := keys(root.visits); strings.Join(got, ",") != "probe,idx" {
		t.Fatalf("root visits = %v, want [probe idx]", got)
	}
}

// TestObservableConstantFoldingNoSideEffect proves parse-time folding evaluates
// only constant subtrees and can never observe a runtime side effect.
func TestObservableConstantFoldingNoSideEffect(t *testing.T) {
	t.Run("constant subtree folds at parse", func(t *testing.T) {
		// 2*3+4 is fully constant; an invalid constant subtree fails at parse.
		if _, err := gval.Full().NewEvaluable(`1 - "x"`); err == nil ||
			!strings.Contains(err.Error(), "parsing error") {
			t.Fatalf("expected a parsing error from folded constants, got %v", err)
		}
	})

	t.Run("function side effect not run at parse", func(t *testing.T) {
		calls := 0
		lang := gval.Full(gval.Function("boom", func() (interface{}, error) {
			calls++
			return 41.0, nil
		}))
		if _, err := lang.NewEvaluable(`1 + boom() == 42`); err != nil {
			t.Fatalf("unexpected parse error: %v", err)
		}
		if calls != 0 {
			t.Fatalf("boom() ran %d time(s) during parsing, want 0", calls)
		}
		value, err := lang.Evaluate(`1 + boom() == 42`, nil)
		if err != nil || value != true {
			t.Fatalf("evaluate value=%v err=%v, want true/<nil>", value, err)
		}
		if calls != 1 {
			t.Fatalf("boom() ran %d time(s) after evaluation, want 1", calls)
		}
	})
}

// observableHidden is a typed value used so the typed-nil pointer error can be
// distinguished from the other failure modes.
type observableHidden struct {
	X int
}

// TestObservableDistinctErrors proves typed-nil, out-of-range and invisible
// fields produce three different, observable errors.
func TestObservableDistinctErrors(t *testing.T) {
	lang := gval.Full()

	t.Run("typed nil pointer", func(t *testing.T) {
		var nilPtr *observableHidden
		root := &pathRecorder{data: map[string]interface{}{"h": nilPtr}}
		_, err := lang.Evaluate(`h.X`, root)
		if err == nil ||
			!strings.Contains(err.Error(), "unknown parameter 'X' on *gval_test.observableHidden") {
			t.Fatalf("want unknown parameter on typed-nil pointer, got %v", err)
		}
	})

	t.Run("typed slice index out of range", func(t *testing.T) {
		root := &pathRecorder{data: map[string]interface{}{"nums": []int{1, 2}}}
		_, err := lang.Evaluate(`nums[5]`, root)
		if err == nil ||
			!strings.Contains(err.Error(), "unknown parameter '5' on []int") {
			t.Fatalf("want unknown parameter for out-of-range slice index, got %v", err)
		}
	})

	t.Run("invisible selector field", func(t *testing.T) {
		root := &pathRecorder{data: map[string]interface{}{}}
		_, err := lang.Evaluate(`private`, root)
		if err == nil ||
			!strings.Contains(err.Error(), `failed to select 'private'`) ||
			!strings.Contains(err.Error(), `invisible field "private"`) {
			t.Fatalf("want wrapped invisible-field selector error, got %v", err)
		}
	})
}

// TestObservableContextCancellation proves an already-cancelled context, and a
// cancellation that happens mid-evaluation, both stop at the next observable
// boundary (a registered function call selects on ctx.Done()).
func TestObservableContextCancellation(t *testing.T) {
	gateCalls := 0
	lang := gval.Full(gval.Function("gate", func() (interface{}, error) {
		gateCalls++
		return true, nil
	}))

	t.Run("already cancelled stops before function and later selector", func(t *testing.T) {
		gateCalls = 0
		root := &pathRecorder{data: map[string]interface{}{"late": 1.0}}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := lang.EvaluateWithContext(ctx, `gate() || late`, root)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		if gateCalls != 0 {
			t.Fatalf("gate ran %d time(s) under a cancelled context, want 0", gateCalls)
		}
		if len(root.visits) != 0 {
			t.Fatalf("later selector visited %v, want no visits", keys(root.visits))
		}
	})

	t.Run("cancelled mid evaluation stops at the next boundary", func(t *testing.T) {
		gateCalls = 0
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		root := &pathRecorder{
			cancel: cancel,
			data:   map[string]interface{}{"step": true, "after": 1.0},
		}
		_, err := lang.EvaluateWithContext(ctx, `step && gate() && after`, root)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		if gateCalls != 0 {
			t.Fatalf("gate ran %d time(s) after cancellation, want 0", gateCalls)
		}
		if got := keys(root.visits); strings.Join(got, ",") != "step" {
			t.Fatalf("visits = %v, want only [step] before the boundary", got)
		}
		if root.visits[0].ctxErr != "" {
			t.Fatalf("context recorded as cancelled at 'step', want it active at that visit")
		}
	})
}
