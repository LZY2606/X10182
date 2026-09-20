package gval_test

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/PaesslerAG/gval"
)

// analysisNode is a controllable, recording implementation of gval.Selector.
// Every SelectGVal call is appended to log together with the context state
// observed at that exact observable boundary.
type analysisNode struct {
	name   string
	fields map[string]interface{}
	log    *[]analysisVisit
}

type analysisVisit struct {
	path    string
	context string
}

func (n *analysisNode) SelectGVal(ctx context.Context, key string) (interface{}, error) {
	visit := analysisVisit{path: n.name + "." + key, context: "active"}
	if err := ctx.Err(); err != nil {
		visit.context = err.Error()
		*n.log = append(*n.log, visit)
		return nil, err
	}
	*n.log = append(*n.log, visit)
	v, ok := n.fields[key]
	if !ok {
		return nil, fmt.Errorf("missing %q on %s", key, n.name)
	}
	return v, nil
}

// analysisExpression contains:
//   - camouflage tokens ("|", "]", EOF),
//   - binary operators of three different precedence levels (* 150, + 120,
//     < 40, == 40, && 21, || 20),
//   - short-circuiting booleans (&& / ||),
//   - a dynamic subscript (root.items[root.kidx]),
//   - a custom gval.Selector (root is an *analysisNode).
const analysisExpression = `1 + 2 * 3 < 100 && flag && root.ok || root.items[root.kidx] == 42`

func analysisParameters(flag, ok bool, log *[]analysisVisit) map[string]interface{} {
	root := &analysisNode{
		name: "root",
		log:  log,
		fields: map[string]interface{}{
			"ok":    ok,
			"kidx":  1.0,
			"items": []interface{}{7.0, 42.0},
		},
	}
	return map[string]interface{}{"flag": flag, "root": root}
}

// TestAnalysis_ParseHasNoSelectorSideEffects proves that parsing the traced
// expression (including folding 1 + 2 * 3 < 100 to a constant) never reaches
// the recording selector.
func TestAnalysis_ParseHasNoSelectorSideEffects(t *testing.T) {
	log := &[]analysisVisit{}
	_, err := gval.Full().NewEvaluable(analysisExpression)
	if err != nil {
		t.Fatalf("NewEvaluable: %v", err)
	}
	if len(*log) != 0 {
		t.Fatalf("selector observed %v during parsing, expected zero accesses", *log)
	}
}

// TestAnalysis_ShortCircuitAndDynamicIndex executes the same compiled
// Evaluable against four parameter combinations. The recorded accesses prove
// that skipped &&/|| branches are never evaluated and that the dynamic
// subscript key is resolved against the original root parameter.
func TestAnalysis_ShortCircuitAndDynamicIndex(t *testing.T) {
	eval, err := gval.Full().NewEvaluable(analysisExpression)
	if err != nil {
		t.Fatalf("NewEvaluable: %v", err)
	}

	tests := []struct {
		name      string
		flag      bool
		ok        bool
		wantValue bool
		wantPaths []string
	}{
		{
			name:      "flag false short circuits the whole && side",
			flag:      false,
			ok:        false,
			wantValue: true,
			wantPaths: []string{"root.items", "root.kidx"},
		},
		{
			name:      "flag true ok true short circuits the || right side",
			flag:      true,
			ok:        true,
			wantValue: true,
			wantPaths: []string{"root.ok"},
		},
		{
			name:      "flag true ok false reaches the dynamic subscript",
			flag:      true,
			ok:        false,
			wantValue: true,
			wantPaths: []string{"root.ok", "root.items", "root.kidx"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := &[]analysisVisit{}
			got, err := eval(context.Background(), analysisParameters(tt.flag, tt.ok, log))
			if err != nil {
				t.Fatalf("eval: %v", err)
			}
			if got != tt.wantValue {
				t.Fatalf("value = %v, want %v", got, tt.wantValue)
			}
			gotPaths := make([]string, len(*log))
			for i, v := range *log {
				gotPaths[i] = v.path
			}
			if !reflect.DeepEqual(gotPaths, tt.wantPaths) {
				t.Fatalf("accesses = %v, want %v", gotPaths, tt.wantPaths)
			}
			for _, v := range *log {
				if v.context != "active" {
					t.Fatalf("access %s observed context %q", v.path, v.context)
				}
			}
		})
	}
}

// TestAnalysis_ConstantFoldingBoundaries pins down when parsing-time
// evaluation is allowed. Built-in constant arithmetic is folded without any
// user side effect; a Function() operand blocks the fold and runs once at
// evaluation time; a custom PrefixOperator on a constant operand is itself
// evaluated at parse time by design, which is documented as the one explicit
// fold boundary that can observe user code.
func TestAnalysis_ConstantFoldingBoundaries(t *testing.T) {
	t.Run("function blocks fold and runs once at evaluation", func(t *testing.T) {
		calls := 0
		lang := gval.NewLanguage(gval.Full(),
			gval.Function("bump", func(x float64) float64 {
				calls++
				return x
			}))
		eval, err := lang.NewEvaluable(`1 + 2 * 3 + bump(4)`)
		if err != nil {
			t.Fatal(err)
		}
		if calls != 0 {
			t.Fatalf("bump called %d times during parse", calls)
		}
		v, err := eval(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if v != 11.0 {
			t.Fatalf("value = %v, want 11", v)
		}
		if calls != 1 {
			t.Fatalf("bump called %d times, want 1", calls)
		}
	})

	t.Run("custom prefix on a constant is folded at parse time", func(t *testing.T) {
		calls := 0
		lang := gval.NewLanguage(gval.Full(),
			gval.PrefixOperator("#", func(c context.Context, v interface{}) (interface{}, error) {
				calls++
				return 42.0, nil
			}))
		eval, err := lang.NewEvaluable(`#5`)
		if err != nil {
			t.Fatal(err)
		}
		if calls != 1 {
			t.Fatalf("prefix called %d times during parse, want 1", calls)
		}
		v, err := eval(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if v != 42.0 {
			t.Fatalf("value = %v, want 42", v)
		}
		if calls != 1 {
			t.Fatalf("prefix called %d times total, want 1", calls)
		}
	})
}

// analysisPlain is a value type without Selector, so variable() falls back
// to the reflect branch of the default selector.
type analysisPlain struct {
	Pub string
	sec string
}

// TestAnalysis_DistinctFailures shows that typed-nil selection, an out of
// range index and an unexported field produce three distinct failures
// (two errors with different messages and one reflect panic).
func TestAnalysis_DistinctFailures(t *testing.T) {
	t.Run("typed nil pointer", func(t *testing.T) {
		var node *analysisPlain
		_, err := gval.Full().Evaluate(`node.Pub`, map[string]interface{}{"node": node})
		if err == nil {
			t.Fatal("want error")
		}
		if !strings.Contains(err.Error(), "unknown parameter 'Pub' on *gval_test.analysisPlain") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("out of range index on typed slice", func(t *testing.T) {
		_, err := gval.Full().Evaluate(`node[5]`, map[string]interface{}{
			"node": []string{"a"},
		})
		if err == nil {
			t.Fatal("want error")
		}
		if !strings.Contains(err.Error(), "unknown parameter '5' on []string") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("unexported field panics through reflect", func(t *testing.T) {
		var recovered interface{}
		func() {
			defer func() { recovered = recover() }()
			_, _ = gval.Full().Evaluate(`node.sec`, map[string]interface{}{
				"node": analysisPlain{Pub: "x"},
			})
		}()
		if recovered == nil {
			t.Fatal("want reflect panic")
		}
		if !strings.Contains(fmt.Sprint(recovered), "unexported field") {
			t.Fatalf("panic = %v", recovered)
		}
	})
}

// TestAnalysis_CanceledContextStopsAtBoundary checks that a context already
// canceled before evaluation stops the run at the first observable boundary:
// the custom Selector for a variable path, and a context-aware Function for
// a call-only expression.
func TestAnalysis_CanceledContextStopsAtBoundary(t *testing.T) {
	t.Run("selector boundary", func(t *testing.T) {
		log := &[]analysisVisit{}
		root := &analysisNode{
			name:   "root",
			log:    log,
			fields: map[string]interface{}{"a": 1.0},
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := gval.Full().EvaluateWithContext(ctx, `root.a == 1`,
			map[string]interface{}{"root": root})
		if err == nil || !strings.Contains(err.Error(), "context canceled") {
			t.Fatalf("error = %v, want context canceled", err)
		}
		if len(*log) != 1 {
			t.Fatalf("accesses = %v, want exactly one boundary stop", *log)
		}
		if (*log)[0].path != "root.a" || (*log)[0].context != "context canceled" {
			t.Fatalf("recorded visit = %+v", (*log)[0])
		}
	})

	t.Run("function boundary", func(t *testing.T) {
		lang := gval.NewLanguage(gval.Full(),
			gval.Function("wait", func(ctx context.Context) (interface{}, error) {
				<-ctx.Done()
				return nil, ctx.Err()
			}))
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(10 * time.Millisecond)
			cancel()
		}()
		_, err := lang.EvaluateWithContext(ctx, `wait()`, nil)
		if err == nil || !strings.Contains(err.Error(), "context canceled") {
			t.Fatalf("error = %v, want context canceled", err)
		}
	})
}

// ExampleAnalysisRecordingSelector is the observable example accompanying
// ANALYSIS.md: the recording selector reports every path access for the
// short-circuited branch.
func Example_recordingSelector() {
	log := &[]analysisVisit{}
	eval, err := gval.Full().NewEvaluable(analysisExpression)
	if err != nil {
		fmt.Println(err)
		return
	}
	value, err := eval(context.Background(), analysisParameters(true, false, log))
	if err != nil {
		fmt.Println(err)
		return
	}
	for _, visit := range *log {
		fmt.Println(visit.path)
	}
	fmt.Println(value)
	// Output:
	// root.ok
	// root.items
	// root.kidx
	// true
}
