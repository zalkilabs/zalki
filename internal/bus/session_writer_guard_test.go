package bus

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// This narrow inventory requires an explicit classification for a new session
// producer. It does not build a general raw-IO dependency framework.
func TestSessionWriterInventory(t *testing.T) {
	expected := map[string]int{
		"session_plugins_mutate.go:SetSessionPlugin": 1,
		"approval.go:rememberApprovalHold":           1, "approval.go:clearApprovalHeld": 1, "approval.go:reconcileSessionHold": 1,
		"hook_lifecycle.go:ApplyLifecycle": 1, "hook_lifecycle.go:ClaimRecovery": 1,
		"hook_registration.go:registerPromptAbsent": 1,
		"listener.go:Arm": 1, "listener.go:Confirm": 1, "listener.go:Close": 1,
		"process.go:rememberOwner": 1, "prune.go:sweepRetired": 1, "prune.go:retireIfDue": 1,
		"event_commit.go:saveSessionWith": 1, "event_commit.go:saveSessionFor": 1,
		"session.go:register": 1, "session.go:updateSession": 1, "session.go:saveSession": 1,
		"session_names.go:rememberNativeConfigDir": 1, "session_names.go:hookName": 1, "task.go:SetTask": 1,
	}
	found := map[string]int{}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		scanSessionProducers(t, path, found)
	}
	if !reflect.DeepEqual(found, expected) {
		t.Fatalf("session producer inventory changed; classify and test the new writer:\ngot%v\nwant%v", found, expected)
	}
}

func scanSessionProducers(t *testing.T, path string, found map[string]int) {
	t.Helper()
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, source, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		key := path + ":" + fn.Name.Name
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			if sessionSaveCall(call.Fun) {
				found[key]++
			}
			return true
		})
		body := string(source[fset.Position(fn.Pos()).Offset:fset.Position(fn.End()).Offset])
		checkDirectSessionWrite(t, key, body, fn.Body)
	}
}

func sessionSaveCall(expr ast.Expr) bool {
	if selector, ok := expr.(*ast.SelectorExpr); ok {
		return selector.Sel.Name == "saveSession"
	}
	if id, ok := expr.(*ast.Ident); ok {
		return id.Name == "saveSessionWith" || id.Name == "saveSessionFor" || id.Name == "savePrivateSessionRecord"
	}
	return false
}

func checkDirectSessionWrite(t *testing.T, key, body string, node *ast.BlockStmt) {
	t.Helper()
	if !strings.Contains(body, `"sessions"`) && !strings.Contains(body, "sessionPath(") && !strings.Contains(body, "sessionRecordPath(") && !strings.Contains(body, "prepareSessionRecord(") && !strings.Contains(body, "sessionRecord") {
		return
	}
	// writeRecord rejects sessions at runtime; retired-record deletion is the
	// explicitly deferred tombstone producer; watch cleanup removes only auxiliary
	// watch paths. These exact-function exemptions admit no new producer functions.
	if key == "event_commit.go:savePrivateSessionRecord" || key == "store_record_merge.go:writeRecord" || key == "prune.go:deleteRetired" || key == "watch.go:consumeWatchesLocked" || key == "watch_prune.go:pruneTargetWatches" {
		return
	}
	ast.Inspect(node, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		name := ""
		switch target := call.Fun.(type) {
		case *ast.SelectorExpr:
			name = target.Sel.Name
		case *ast.Ident:
			name = target.Name
		}
		switch name {
		case "writeJSON", "WriteAtomic", "WriteFile", "Rename", "Remove", "RemoveAll":
			t.Errorf("%s directly mutates a session path; use saveSession", key)
		}
		return true
	})
}
