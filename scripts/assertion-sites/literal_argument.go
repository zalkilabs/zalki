package main

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"strconv"
)

// This is a closed binding for the two reviewed Z71F2 callbacks, not general
// interprocedural constant propagation. The exact helper body also excludes
// receiver/formal rebinding, shadowing, aliases, and variadic forwarding.
type literalArgument struct {
	Root          string `json:"root"`
	Callback      string `json:"callback"`
	Parameter     string `json:"parameter"`
	ArgumentIndex int    `json:"argument_index"`
	ParameterType string `json:"parameter_type"`
	TestArgument  string `json:"test_argument"`
	ExpectedCalls int    `json:"expected_call_sites"`
}

type literalBinding struct {
	Request            literalArgument `json:"request"`
	FormalNames        []string        `json:"formal_names"`
	FormalTypes        []string        `json:"formal_types"`
	PhysicalFatalSites int             `json:"physical_fatal_sites"`
	InstantiatedSites  int             `json:"instantiated_argument_sites"`
	SiblingSites       int             `json:"registered_sibling_sites"`
	SourceFatalLines   []int           `json:"source_fatal_lines"`
	CallerReportLines  []int           `json:"caller_report_lines"`
	RuntimeTest        string          `json:"runtime_test"`
}

const loudStopRoot = "TestHostDisableEndsQuietlyWhenPluginExitsNonZero"
const loudStopFile = "internal/plugins/host_disable_test.go"
const loudStopHelper = `package plugins
func checkLoudStop(t *testing.T, mode string, receive bool, failure string) {
 t.Helper()
 store, done := startDisableHost(t, mode, receive)
 disableHost(t, store)
 if hostResult(t, done, 8*time.Second) == nil { t.Fatal(failure) }
}`

func literalText(expression ast.Expr) (string, bool) {
	value, ok := expression.(*ast.BasicLit)
	if !ok || value.Kind != token.STRING {
		return "", false
	}
	text, err := strconv.Unquote(value.Value)
	return text, err == nil
}

func exactTestingParameter(function *ast.FuncDecl) bool {
	if function == nil || function.Recv != nil || function.Type.Results != nil || function.Type.TypeParams != nil ||
		function.Type.Params == nil || len(function.Type.Params.List) != 1 {
		return false
	}
	field := function.Type.Params.List[0]
	shape, err := expressionShape(field.Type)
	return err == nil && shape == "*testing.T" && len(field.Names) == 1 && field.Names[0].Name == "t"
}

func loudStopSpec(callback string) (mode, receive, literal string, ok bool) {
	switch callback {
	case "signal death":
		return "selfterm-on-code", "true", "a plugin that died by a signal at disable was reported as clean", true
	case "ignored stop":
		return "ignore-on-code", "false", "a plugin that ignored the stop was reported as clean", true
	}
	return "", "", "", false
}

func closedLoudRequest(callback string) request {
	_, _, literal, _ := loudStopSpec(callback)
	return request{File: loudStopFile, Function: "checkLoudStop", Literal: literal, Call: "t.Fatal(failure)",
		ExpectedSites: 1, ExpectedFileSites: 1, RequireHelpers: true,
		LiteralArgument: &literalArgument{Root: loudStopRoot, Callback: callback, Parameter: "failure", ArgumentIndex: 3,
			ParameterType: "string", TestArgument: "t", ExpectedCalls: 1},
		Chain: []caller{{Kind: "helper", Caller: loudStopRoot, Callee: "checkLoudStop", ExpectedCalls: 2}}}
}

func validLoudRequest(r request, expected string) bool {
	if r.LiteralArgument == nil {
		return false
	}
	_, _, _, known := loudStopSpec(r.LiteralArgument.Callback)
	if !known || expected != "t.Fatal(failure)" {
		return false
	}
	r.Mutation = nil
	return reflect.DeepEqual(r, closedLoudRequest(r.LiteralArgument.Callback))
}

func closedLoudHelper(file *ast.File, functions map[string]*ast.FuncDecl) bool {
	imports := 0
	for _, imp := range file.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err == nil && path == "testing" && imp.Name == nil {
			imports++
		}
	}
	helper := functions["checkLoudStop"]
	if imports != 1 || helper == nil {
		return false
	}
	canonical, err := parser.ParseFile(token.NewFileSet(), "closed.go", loudStopHelper, parser.SkipObjectResolution)
	if err != nil {
		return false
	}
	want, err := expressionShape(canonical.Decls[0])
	if err != nil {
		return false
	}
	got, err := expressionShape(helper)
	return err == nil && got == want
}

func uniqueLoudFatal(fset *token.FileSet, functions map[string]*ast.FuncDecl) []int {
	count := 0
	var result []int
	for name, function := range functions {
		if function.Body == nil {
			continue
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			shape, err := expressionShape(call)
			if err == nil && shape == "t.Fatal(failure)" {
				count++
				if name == "checkLoudStop" {
					result = lines(fset, call)
				}
			}
			return true
		})
	}
	if count != 1 {
		return nil
	}
	return result
}

func closedRootRuns(root *ast.FuncDecl) ([]*ast.CallExpr, error) {
	if !exactTestingParameter(root) || root.Body == nil || helperMarked(root) {
		return nil, errors.New("literal binding root is not a fresh testing root")
	}
	var runs []*ast.CallExpr
	for _, statement := range root.Body.List {
		expression, ok := statement.(*ast.ExprStmt)
		if !ok {
			return nil, errors.New("literal binding root has non-registration statements")
		}
		run, ok := expression.X.(*ast.CallExpr)
		if !ok || !testMethod(run, "t", "Run") || len(run.Args) != 2 || run.Ellipsis.IsValid() {
			return nil, errors.New("literal binding root has an unregistered callback")
		}
		if _, ok := literalText(run.Args[0]); !ok {
			return nil, errors.New("literal callback label is not literal")
		}
		runs = append(runs, run)
	}
	return runs, nil
}

func loudCalls(closure *ast.FuncLit) []*ast.CallExpr {
	var calls []*ast.CallExpr
	ast.Inspect(closure.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if ok {
			name, named := call.Fun.(*ast.Ident)
			if named && name.Name == "checkLoudStop" {
				calls = append(calls, call)
			}
		}
		return true
	})
	return calls
}

func closedLoudArguments(call *ast.CallExpr, label string) bool {
	mode, receive, literal, known := loudStopSpec(label)
	if !known || len(call.Args) != 4 || call.Ellipsis.IsValid() {
		return false
	}
	actual := make([]string, 4)
	for index, arg := range call.Args {
		shape, err := expressionShape(arg)
		if err != nil {
			return false
		}
		actual[index] = shape
	}
	return reflect.DeepEqual(actual, []string{"t", strconv.Quote(mode), receive, strconv.Quote(literal)})
}

func closedLoudCallback(run *ast.CallExpr) (*ast.CallExpr, string, error) {
	label, _ := literalText(run.Args[0])
	_, _, _, known := loudStopSpec(label)
	closure, inline := run.Args[1].(*ast.FuncLit)
	if !inline {
		if known {
			return nil, label, errors.New("literal callback is not the registered fresh function")
		}
		return nil, label, nil
	}
	callback := &ast.FuncDecl{Type: closure.Type, Body: closure.Body}
	if !exactTestingParameter(callback) || helperMarked(callback) {
		return nil, label, errors.New("literal callback testing receiver differs")
	}
	calls := loudCalls(closure)
	if !known {
		if len(calls) > 0 {
			return nil, label, errors.New("unregistered literal caller")
		}
		return nil, label, nil
	}
	if len(calls) != 1 || len(closure.Body.List) != 1 {
		return nil, label, errors.New("ambiguous or rebound literal callback")
	}
	statement, ok := closure.Body.List[0].(*ast.ExprStmt)
	if !ok || statement.X != calls[0] || !closedLoudArguments(calls[0], label) {
		return nil, label, errors.New("literal arguments differ from exact formal tuple")
	}
	return calls[0], label, nil
}

func loudReportLines(fset *token.FileSet, root *ast.FuncDecl, wanted string) ([]int, error) {
	runs, err := closedRootRuns(root)
	if err != nil {
		return nil, err
	}
	siblings := map[string]bool{}
	var report []int
	for _, run := range runs {
		call, label, err := closedLoudCallback(run)
		if err != nil {
			return nil, err
		}
		if call == nil {
			continue
		}
		if siblings[label] {
			return nil, errors.New("duplicate literal callback")
		}
		siblings[label] = true
		if label == wanted {
			report = lines(fset, call)
		}
	}
	if len(siblings) != 2 || len(report) == 0 {
		return nil, errors.New("literal callback inventory differs")
	}
	return report, nil
}

func resolveLiteralArgument(fset *token.FileSet, file *ast.File, functions map[string]*ast.FuncDecl, r request, expected string) (response, error) {
	if !validLoudRequest(r, expected) || !closedLoudHelper(file, functions) {
		return response{}, errors.New("closed Z71F2 literal/formal request or helper differs")
	}
	fatalLines := uniqueLoudFatal(fset, functions)
	if len(fatalLines) != 1 {
		return response{}, errors.New("physical literal fatal inventory differs")
	}
	reports, err := loudReportLines(fset, functions[loudStopRoot], r.LiteralArgument.Callback)
	if err != nil {
		return response{}, err
	}
	binding := &literalBinding{Request: *r.LiteralArgument, FormalNames: []string{"t", "mode", "receive", "failure"},
		FormalTypes: []string{"*testing.T", "string", "bool", "string"}, PhysicalFatalSites: 1, InstantiatedSites: 1,
		SiblingSites: 2, SourceFatalLines: fatalLines, CallerReportLines: reports,
		RuntimeTest: loudStopRoot + "/" + map[string]string{"signal death": "signal_death", "ignored stop": "ignored_stop"}[r.LiteralArgument.Callback]}
	return response{File: r.File, Function: r.Function, Literal: r.Literal, ReviewedCall: r.Call,
		AssertionLines: fatalLines, ReportLines: reports, FileSites: 1, AssertionSites: 1, LiteralBinding: binding}, nil
}
