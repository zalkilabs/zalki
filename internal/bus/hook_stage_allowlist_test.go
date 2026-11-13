package bus

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestEveryHookStageIsLoggable discovers stages from production source rather
// than duplicating its list. Each stage must survive the real log allowlist.
func TestEveryHookStageIsLoggable(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	stages := map[string]bool{}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		if err := collectTraceStages(file, stages); err != nil {
			t.Fatal(err)
		}
	}
	if len(stages) == 0 {
		t.Fatal("found no hook stage assignments: source inventory is empty")
	}
	for stage := range stages {
		t.Run(stage, func(t *testing.T) {
			attrs := (Store{}).appendAgentAndHookAttrs(nil, eventData{HookStage: stage})
			for _, attr := range attrs {
				if attr.Key == "hook_stage" && attr.Value.String() == stage {
					return
				}
			}
			t.Fatalf("hook stage %q is set in production but dropped by the log allowlist", stage)
		})
	}
}

func collectTraceStages(file *ast.File, stages map[string]bool) error {
	var err error
	ast.Inspect(file, func(node ast.Node) bool {
		if err != nil {
			return false
		}
		err = collectTraceStage(node, stages)
		return err == nil
	})
	return err
}

func collectTraceStage(node ast.Node, stages map[string]bool) error {
	switch n := node.(type) {
	case *ast.AssignStmt:
		return collectAssignedTraceStages(n, stages)
	case *ast.CompositeLit:
		if name, ok := n.Type.(*ast.Ident); ok && name.Name == "hookTrace" {
			return collectInitializedTraceStages(n, stages)
		}
	}
	return nil
}

func collectAssignedTraceStages(n *ast.AssignStmt, stages map[string]bool) error {
	for i, lhs := range n.Lhs {
		if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "stage" {
			if n.Tok != token.ASSIGN {
				return errors.New("hook stage compound assignment is unsupported; update source inventory")
			}
			if i >= len(n.Rhs) {
				return errors.New("hook stage uses a multi-result assignment; update source inventory")
			}
			if err := collectTraceStageValue(n.Rhs[i], stages); err != nil {
				return err
			}
		}
	}
	return nil
}

func collectInitializedTraceStages(n *ast.CompositeLit, stages map[string]bool) error {
	for _, elt := range n.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			return errors.New("hookTrace initializer must use named fields; update source inventory")
		}
		if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "stage" {
			if err := collectTraceStageValue(kv.Value, stages); err != nil {
				return err
			}
		}
	}
	return nil
}

func collectTraceStageValue(expr ast.Expr, stages map[string]bool) error {
	literal, ok := expr.(*ast.BasicLit)
	if !ok || literal.Kind != token.STRING {
		return errors.New("hook stage is not a string literal; update source inventory before adding dynamic stages")
	}
	stage, err := strconv.Unquote(literal.Value)
	if err != nil {
		return fmt.Errorf("invalid hook stage literal %q: %w", literal.Value, err)
	}
	if stage == "" {
		return errors.New("hook stage literal must not be empty")
	}
	stages[stage] = true
	return nil
}

func TestHookStageInventoryRejectsDynamicForms(t *testing.T) {
	for _, tc := range []struct {
		name, source, wantError string
		wantStages              int
	}{
		{"compound", `trace.stage += "handler"`, "hook stage compound assignment", 0},
		{"unkeyed", `_ = hookTrace{"", "", "new_stage", ""}`, "hookTrace initializer must use named fields", 0},
		{"keyed", `_ = hookTrace{stage: "adapter"}; trace.stage = "handler"`, "", 2},
		{"expression", `trace.stage = "adap" + "ter"`, "hook stage is not a string literal", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file, err := parser.ParseFile(token.NewFileSet(), "fixture.go", "package bus; func fixture() { "+tc.source+" }", 0)
			if err != nil {
				t.Fatal(err)
			}
			stages := map[string]bool{}
			err = collectTraceStages(file, stages)
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("stage inventory must reject %s: %v", tc.name, err)
				}
			} else if err != nil || len(stages) != tc.wantStages || !stages["adapter"] || !stages["handler"] {
				t.Fatalf("keyed stage inventory = %v, %v", stages, err)
			}
		})
	}
}
