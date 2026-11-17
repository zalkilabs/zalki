// Command assertion-sites binds a focused mutation's intended assertion and
// testing.T.Helper caller positions to the current parsed test source.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type caller struct {
	ExpectedCalls int    `json:"expected_call_sites,omitempty"`
	Kind          string `json:"kind,omitempty"`
	Caller        string `json:"caller"`
	Callee        string `json:"callee"`
	CallerHelper  bool   `json:"caller_is_helper"`
}

type request struct {
	LiteralArgument   *literalArgument `json:"literal_argument,omitempty"`
	File              string           `json:"file"`
	Function          string           `json:"assertion_function"`
	Literal           string           `json:"fatal_literal"`
	Call              string           `json:"fatal_call"`
	ExpectedSites     int              `json:"expected_assertion_sites"`
	ExpectedFileSites int              `json:"expected_file_assertion_sites"`
	Chain             []caller         `json:"call_chain"`
	RequireHelpers    bool             `json:"require_helper_markers_in_callees"`
	CallbackDirect    bool             `json:"callback_direct"`
	Mutation          *mutationRequest `json:"mutation,omitempty"`
}

type mutationRequest struct {
	Kind   string `json:"declaration_kind,omitempty"`
	File   string `json:"path"`
	Symbol string `json:"symbol"`
	Old    string `json:"old"`
	// New is the runner's typed replacement text. bindMutation proves the old
	// preimage; the runner checks/applies/restores the complete old/new tuple.
	New   string `json:"new"`
	Count int    `json:"required_occurrences"`
}

type mutationProof struct {
	Kind   string `json:"declaration_kind,omitempty"`
	File   string `json:"file"`
	Symbol string `json:"symbol"`
	SHA256 string `json:"source_sha256"`
	Offset int    `json:"offset"`
}

type response struct {
	Chain              []caller        `json:"call_chain,omitempty"`
	LiteralBinding     *literalBinding `json:"literal_argument_binding,omitempty"`
	NamedCallbackSites int             `json:"named_callback_sites,omitempty"`
	File               string          `json:"file"`
	SHA256             string          `json:"source_sha256"`
	Function           string          `json:"function"`
	Literal            string          `json:"literal"`
	AssertionLines     []int           `json:"assertion_lines"`
	ReportLines        []int           `json:"report_lines"`
	FileSites          int             `json:"file_sites"`
	AssertionSites     int             `json:"assertion_sites"`
	HelperChainValid   bool            `json:"helper_chain_valid"`
	ReviewedCall       string          `json:"reviewed_call"`
	CallbackSites      int             `json:"callback_sites"`
	Mutation           *mutationProof  `json:"mutation,omitempty"`
}

func testArgument(f *ast.FuncDecl) string {
	for _, parameter := range f.Type.Params.List {
		pointer, ok := parameter.Type.(*ast.StarExpr)
		if !ok {
			continue
		}
		selected, ok := pointer.X.(*ast.SelectorExpr)
		if !ok {
			continue
		}
		pkg, ok := selected.X.(*ast.Ident)
		if ok && pkg.Name == "testing" && selected.Sel.Name == "T" && len(parameter.Names) == 1 {
			return parameter.Names[0].Name
		}
	}
	return ""
}

func testMethod(call *ast.CallExpr, argument, method string) bool {
	selected, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || selected.Sel.Name != method {
		return false
	}
	receiver, ok := selected.X.(*ast.Ident)
	return ok && argument != "" && receiver.Name == argument
}

func helperMarked(f *ast.FuncDecl) bool {
	if f.Body == nil {
		return false
	}
	for _, statement := range f.Body.List {
		expression, ok := statement.(*ast.ExprStmt)
		if !ok {
			continue
		}
		call, ok := expression.X.(*ast.CallExpr)
		if ok && testMethod(call, testArgument(f), "Helper") {
			return true
		}
	}
	return false
}

func assertion(call *ast.CallExpr, argument, literal string) bool {
	if !testMethod(call, argument, "Fatal") && !testMethod(call, argument, "Fatalf") &&
		!testMethod(call, argument, "Error") && !testMethod(call, argument, "Errorf") {
		return false
	}
	for _, arg := range call.Args {
		value, ok := arg.(*ast.BasicLit)
		if !ok || value.Kind != token.STRING {
			continue
		}
		text, err := strconv.Unquote(value.Value)
		if err == nil && text == literal {
			return true
		}
	}
	return false
}

func lines(fset *token.FileSet, call *ast.CallExpr) []int {
	result := []int{}
	for line := fset.Position(call.Pos()).Line; line <= fset.Position(call.End()).Line; line++ {
		result = append(result, line)
	}
	return result
}

func callLines(fset *token.FileSet, f *ast.FuncDecl, callee string) ([]int, int) {
	result, count := []int{}, 0
	ast.Inspect(f.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		name, ok := call.Fun.(*ast.Ident)
		if ok && name.Name == callee {
			result = append(result, lines(fset, call)...)
			count++
		}
		return true
	})
	return result, count
}

func directTestCallback(f *ast.FuncDecl, call *ast.CallExpr) bool {
	var inner *ast.FuncLit
	ast.Inspect(f.Body, func(node ast.Node) bool {
		closure, ok := node.(*ast.FuncLit)
		if ok && closure.Pos() < call.Pos() && call.End() < closure.End() && (inner == nil || inner.Pos() < closure.Pos()) {
			inner = closure
		}
		return true
	})
	if inner == nil {
		return false
	}
	function := &ast.FuncDecl{Type: inner.Type, Body: inner.Body}
	argument := testArgument(function)
	return argument != "" && !helperMarked(function) && registeredTestCallback(f, inner) && testAssertionReceiver(call, argument)
}

func testAssertionReceiver(call *ast.CallExpr, argument string) bool {
	return testMethod(call, argument, "Fatal") || testMethod(call, argument, "Fatalf") ||
		testMethod(call, argument, "Error") || testMethod(call, argument, "Errorf")
}

func registeredTestCallback(f *ast.FuncDecl, inner *ast.FuncLit) bool {
	registered := false
	ast.Inspect(f.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if ok && testMethod(call, testArgument(f), "Run") && len(call.Args) == 2 && !call.Ellipsis.IsValid() {
			registered = registered || call.Args[1] == inner
		}
		return true
	})
	return registered
}

func resolveChain(fset *token.FileSet, functions map[string]*ast.FuncDecl, r request, direct []int) ([]int, error) {
	if len(r.Chain) == 0 {
		return direct, nil
	}
	if len(r.Chain) > 0 && r.Chain[0].Kind == "run_callback" {
		return namedCallbackLines(fset, functions, &r, direct)
	}
	return helperChainLines(fset, functions, &r)
}

func declarationMap(file *ast.File) (map[string]*ast.FuncDecl, error) {
	functions := map[string]*ast.FuncDecl{}
	for _, declaration := range file.Decls {
		f, ok := declaration.(*ast.FuncDecl)
		if !ok {
			continue
		}
		name := f.Name.Name
		if f.Recv != nil {
			name = fmt.Sprintf("@method:%d", f.Pos())
		}
		if functions[name] != nil {
			return nil, errors.New("ambiguous assertion function")
		}
		functions[name] = f
	}
	return functions, nil
}

func expressionShape(node ast.Node) (string, error) {
	var out bytes.Buffer
	err := printer.Fprint(&out, token.NewFileSet(), node)
	return out.String(), err
}

type assertionCounter struct {
	fset     *token.FileSet
	request  request
	expected string
	result   response
}

func (c *assertionCounter) record(name string, f *ast.FuncDecl, call *ast.CallExpr) error {
	if !assertion(call, testArgument(f), c.request.Literal) {
		return nil
	}
	c.result.FileSites++
	if name != c.request.Function {
		return nil
	}
	shape, err := expressionShape(call)
	if err != nil {
		return err
	}
	if shape != c.expected {
		return nil
	}
	c.result.AssertionSites++
	if directTestCallback(f, call) {
		c.result.CallbackSites++
	}
	c.result.AssertionLines = append(c.result.AssertionLines, lines(c.fset, call)...)
	return nil
}

func sourceAssertions(fset *token.FileSet, functions map[string]*ast.FuncDecl, r request, expected string) (response, error) {
	counter := assertionCounter{fset: fset, request: r, expected: expected, result: response{File: r.File, Function: r.Function, Literal: r.Literal, ReviewedCall: r.Call, AssertionLines: []int{}}}
	for name, f := range functions {
		if f.Body == nil {
			continue
		}
		var shapeErr error
		ast.Inspect(f.Body, func(node ast.Node) bool {
			if shapeErr != nil {
				return false
			}
			if call, ok := node.(*ast.CallExpr); ok {
				shapeErr = counter.record(name, f, call)
			}
			return shapeErr == nil
		})
		if shapeErr != nil {
			return response{}, shapeErr
		}
	}
	if counter.result.AssertionSites != r.ExpectedSites || counter.result.FileSites != r.ExpectedFileSites {
		return response{}, errors.New("assertion site inventory differs from reviewed set")
	}
	return counter.result, nil
}

func completeRequest(r *request, functions map[string]*ast.FuncDecl) bool {
	return r.ExpectedSites >= 1 && r.ExpectedFileSites >= r.ExpectedSites && r.Literal != "" &&
		functions[r.Function] != nil && r.Call != ""
}

func resolve(raw []byte, r request) (response, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, r.File, raw, parser.SkipObjectResolution)
	if err != nil {
		return response{}, err
	}
	functions, err := declarationMap(file)
	if err != nil {
		return response{}, err
	}
	if !completeRequest(&r, functions) {
		return response{}, errors.New("incomplete reviewed assertion identity")
	}
	expression, err := parser.ParseExpr(r.Call)
	if err != nil {
		return response{}, err
	}
	expected, err := expressionShape(expression)
	if err != nil {
		return response{}, err
	}
	var result response
	if r.LiteralArgument != nil {
		result, err = resolveLiteralArgument(fset, file, functions, r, expected)
	} else {
		result, err = sourceAssertions(fset, functions, r, expected)
		if err == nil {
			result.ReportLines, err = resolveChain(fset, functions, r, result.AssertionLines)
		}
		if err == nil && r.CallbackDirect {
			if len(r.Chain) == 0 || result.CallbackSites != result.AssertionSites {
				return response{}, errors.New("direct callback attribution lacks a fresh non-helper testing callback")
			}
			result.ReportLines = result.AssertionLines
		}
		for _, edge := range r.Chain {
			if edge.Kind != "run_callback" {
				break
			}
			result.NamedCallbackSites++
		}
	}
	if err != nil {
		return response{}, err
	}
	result.Chain = append([]caller(nil), r.Chain...)
	result.HelperChainValid = true
	sum := sha256.Sum256(raw)
	result.SHA256 = hex.EncodeToString(sum[:])
	sort.Ints(result.AssertionLines)
	sort.Ints(result.ReportLines)
	return result, nil
}

func rootedSource(root, name string) ([]byte, error) {
	if !filepath.IsLocal(name) || strings.Contains(name, "\\") {
		return nil, errors.New("assertion source must be relative")
	}
	current := root
	for _, part := range strings.Split(filepath.ToSlash(name), "/") {
		if part == ".git" {
			return nil, errors.New("source metadata is not an assertion file")
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("assertion source links are unsupported")
		}
	}
	opened, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	raw, readErr := opened.ReadFile(name)
	return raw, errors.Join(readErr, opened.Close())
}

func bindMutation(raw []byte, r mutationRequest) (*mutationProof, error) {
	if r.Count != 1 || strings.TrimSpace(r.Old) == "" || strings.Count(string(raw), r.Old) != 1 {
		return nil, errors.New("mutation preimage must occur exactly once")
	}
	offset := strings.Index(string(raw), r.Old)
	if r.File != ".goreleaser.yaml" || r.Symbol != "builds[0].ldflags" {
		if err := mutationDeclaration(raw, r, offset); err != nil {
			return nil, err
		}
	} else if r.Kind != "" && r.Kind != "function" {
		return nil, errors.New("configuration mutation cannot claim a Go declaration")
	}
	sum := sha256.Sum256(raw)
	return &mutationProof{File: r.File, Kind: r.Kind, Symbol: r.Symbol, SHA256: hex.EncodeToString(sum[:]), Offset: offset}, nil
}

func run(root string, in io.Reader, out io.Writer) error {
	var r request
	decoder := json.NewDecoder(in)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&r); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("trailing assertion request data")
	}
	raw, err := rootedSource(root, r.File)
	if err != nil {
		return err
	}
	result, err := resolve(raw, r)
	if err != nil {
		return err
	}
	if r.Mutation != nil {
		before, err := rootedSource(root, r.Mutation.File)
		if err != nil {
			return err
		}
		result.Mutation, err = bindMutation(before, *r.Mutation)
		if err != nil {
			return err
		}
	}
	var encoded bytes.Buffer
	if err := json.NewEncoder(&encoded).Encode(result); err != nil {
		return err
	}
	_, err = io.Copy(out, &encoded)
	return err
}

func main() {
	root := flag.String("root", ".", "immutable source root")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: assertion-sites -root SOURCE < REQUEST.json")
		os.Exit(2)
	}
	if err := run(*root, os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
