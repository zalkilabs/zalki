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

func namedCallbackLines(functions map[string]*ast.FuncDecl, r *request, direct []int) ([]int, error) {
	edge := r.Chain[0]
	from, to := functions[edge.Caller], functions[edge.Callee]
	if from == nil || to == nil || from.Body == nil || to.Body == nil || edge.CallerHelper || helperMarked(from) || helperMarked(to) ||
		testArgument(from) == "" || testArgument(to) == "" || edge.Callee != r.Function ||
		r.RequireHelpers || r.CallbackDirect {
		return nil, errors.New("named callback lineage differs from its fresh non-helper source")
	}
	if namedCallbackCount(from, edge.Callee) != 1 {
		return nil, errors.New("named callback must have exactly one current testing Run reference")
	}
	return direct, nil
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
		if count != 1 {
			return nil, errors.New("helper call must resolve to exactly one current source site")
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
