package main

import (
	"errors"
	"go/ast"
	"go/token"
	"strconv"
)

func namedCallbackCount(f *ast.FuncDecl, callee string) int {
	count := 0
	ast.Inspect(f.Body, func(node ast.Node) bool {
		if _, nested := node.(*ast.FuncLit); nested {
			return false
		}
		call, ok := node.(*ast.CallExpr)
		if !ok || !testMethod(call, testArgument(f), "Run") || len(call.Args) != 2 {
			return true
		}
		name, ok := call.Args[1].(*ast.Ident)
		label, literal := call.Args[0].(*ast.BasicLit)
		if !ok || name.Name != callee || !literal || label.Kind != token.STRING {
			return true
		}
		if _, err := strconv.Unquote(label.Value); err == nil {
			count++
		}
		return true
	})
	return count
}

func callbackEdge(functions map[string]*ast.FuncDecl, edge caller, nested bool) error {
	if edge.ExpectedCalls != 0 && edge.ExpectedCalls != 1 {
		return errors.New("named callback registration count must be one")
	}
	from, to := functions[edge.Caller], functions[edge.Callee]
	if from == nil || to == nil || from.Body == nil || to.Body == nil || edge.CallerHelper || helperMarked(from) ||
		testArgument(from) == "" || testArgument(to) == "" || (helperMarked(to) && !nested) {
		return errors.New("named callback lineage differs from its fresh source")
	}
	if namedCallbackCount(from, edge.Callee) != 1 {
		return errors.New("named callback must have exactly one current testing Run reference")
	}
	return nil
}

func namedCallbackLines(fset *token.FileSet, functions map[string]*ast.FuncDecl, r *request, direct []int) ([]int, error) {
	named := 0
	seen := map[string]bool{r.Chain[0].Caller: true}
	for index, edge := range r.Chain {
		if edge.Kind != "run_callback" {
			break
		}
		if (index > 0 && r.Chain[index-1].Callee != edge.Caller) || seen[edge.Callee] {
			return nil, errors.New("disconnected or cyclic registered callback path")
		}
		nested := index == len(r.Chain)-1 && r.CallbackDirect
		if err := callbackEdge(functions, edge, nested); err != nil {
			return nil, err
		}
		seen[edge.Callee] = true
		named++
	}
	if named == len(r.Chain) {
		if r.Chain[named-1].Callee != r.Function || r.RequireHelpers {
			return nil, errors.New("registered callback path misses the intended assertion")
		}
		return direct, nil
	}
	if r.CallbackDirect || r.Chain[named].Caller != r.Chain[named-1].Callee {
		return nil, errors.New("registered callback and helper path differ")
	}
	tail := *r
	tail.Chain = r.Chain[named:]
	return helperChainLines(fset, functions, &tail)
}

func helperEdge(functions map[string]*ast.FuncDecl, edge caller, index int) (*ast.FuncDecl, error) {
	if edge.Kind != "" && edge.Kind != "helper" {
		return nil, errors.New("unknown or mixed assertion lineage")
	}
	from, to := functions[edge.Caller], functions[edge.Callee]
	if from == nil || to == nil || from.Body == nil || helperMarked(from) != edge.CallerHelper || !helperMarked(to) {
		return nil, errors.New("assertion caller/helper chain differs from reviewed source")
	}
	if index == 0 && edge.CallerHelper {
		return nil, errors.New("caller chain must start at a non-helper test")
	}
	return from, nil
}

func helperChainLines(fset *token.FileSet, functions map[string]*ast.FuncDecl, r *request) ([]int, error) {
	var reported []int
	for index, edge := range r.Chain {
		from, err := helperEdge(functions, edge, index)
		if err != nil {
			return nil, err
		}
		if index > 0 && r.Chain[index-1].Callee != edge.Caller {
			return nil, errors.New("disconnected helper chain")
		}
		found, count := callLines(fset, from, edge.Callee)
		expected := edge.ExpectedCalls
		if expected == 0 {
			expected = 1
		}
		if expected < 1 || count != expected {
			return nil, errors.New("helper call count differs from the exact reviewed source")
		}
		if index == 0 {
			reported = found
		}
	}
	if r.Chain[len(r.Chain)-1].Callee != r.Function || !r.RequireHelpers {
		return nil, errors.New("helper chain does not end at the intended assertion")
	}
	return reported, nil
}
