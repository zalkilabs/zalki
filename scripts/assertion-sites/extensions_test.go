package main

import (
	"slices"
	"strings"
	"testing"
)

func TestMutationValuesStayInsideNamedPackageDeclaration(t *testing.T) {
	const source = "package fixture\nconst (\n first = 1\n wanted = 3 // reviewed label\n)\nvar service = build(`pattern`)\nconst grace = 3\n"
	cases := []mutationRequest{
		{File: "fixture.go", Kind: "const", Symbol: "wanted", Old: " wanted = 3 // reviewed label", Count: 1},
		{File: "fixture.go", Kind: "var", Symbol: "service", Old: "`pattern`", Count: 1},
		{File: "fixture.go", Kind: "const", Symbol: "grace", Old: "const grace = 3", Count: 1},
	}
	for _, request := range cases {
		proof, err := bindMutation([]byte(source), request)
		if err != nil || proof.Symbol != request.Symbol || proof.Kind != request.Kind {
			t.Fatalf("named package value was not bound: %+v, %v", proof, err)
		}
	}
	checkAccessFieldMutation(t)
}

func TestMutationValueBoundsRejectAdjacentAndWrongKinds(t *testing.T) {
	const source = "package fixture\nconst ( first = 1; wanted = 3 )\nvar other = 4\nfunc method() { const inner = 5 }\n"
	cases := []mutationRequest{
		{Kind: "const", Symbol: "wanted", Old: "first = 1", Count: 1},
		{Kind: "const", Symbol: "wanted", Old: "first = 1; wanted = 3", Count: 1},
		{Kind: "var", Symbol: "wanted", Old: "wanted = 3", Count: 1},
		{Kind: "const", Symbol: "inner", Old: "inner = 5", Count: 1},
		{Kind: "function", Symbol: "wanted", Old: "wanted = 3", Count: 1},
		{Kind: "arbitrary", Symbol: "wanted", Old: "wanted = 3", Count: 1},
	}
	for _, request := range cases {
		request.File = "fixture.go"
		if _, err := bindMutation([]byte(source), request); err == nil {
			t.Fatalf("unrelated package value was accepted: %+v", request)
		}
	}
	rejectAccessFieldMutations(t)
}

const accessFieldSource = "package pluginfeed\ntype PluginAccess struct {\n Status bool\n}\n"

func accessFieldRequest() mutationRequest {
	return mutationRequest{Kind: "type", File: "internal/pluginfeed/plugin_access.go", Symbol: "PluginAccess",
		Old: accessFieldMutationOld, New: accessFieldMutationNew, Count: 1}
}

func checkAccessFieldMutation(t *testing.T) {
	t.Helper()
	r := accessFieldRequest()
	proof, err := bindMutation([]byte(accessFieldSource), r)
	if err != nil || proof.File != r.File || proof.Symbol != r.Symbol || proof.Kind != "type" ||
		proof.Offset != strings.Index(accessFieldSource, r.Old) || proof.SHA256 == "" {
		t.Fatalf("reviewed named struct insertion was not bound: %+v %v", proof, err)
	}
}

func rejectAccessFieldMutations(t *testing.T) {
	t.Helper()
	edits := []func(*mutationRequest){
		func(r *mutationRequest) { r.File = "../plugin_access.go" },
		func(r *mutationRequest) { r.File = "internal/pluginfeed/other.go" },
		func(r *mutationRequest) { r.Symbol = "Other" },
		func(r *mutationRequest) { r.Kind = "function" },
		func(r *mutationRequest) { r.Kind = "var" },
		func(r *mutationRequest) { r.Old = "PluginAccess struct {" },
		func(r *mutationRequest) { r.New = "" },
		func(r *mutationRequest) { r.Count = 2 },
	}
	for _, edit := range edits {
		r := accessFieldRequest()
		edit(&r)
		if _, err := bindMutation([]byte(accessFieldSource), r); err == nil {
			t.Fatalf("unreviewed struct tuple accepted: %+v", r)
		}
	}
	for _, source := range []string{
		strings.Replace(accessFieldSource, "package pluginfeed", "package other", 1),
		"package pluginfeed\ntype PluginAccess = struct { Status bool }\n",
		"package pluginfeed\ntype (PluginAccess struct { Status bool })\n",
		"package pluginfeed\ntype PluginAccess[T any] struct { Status bool }\n",
		"package pluginfeed\nfunc local() { type PluginAccess struct { Status bool } }\n",
		"package pluginfeed\n// type PluginAccess struct {\ntype Other struct { Status bool }\n",
		accessFieldSource + "type PluginAccess struct { Other bool }\n",
	} {
		if _, err := bindMutation([]byte(source), accessFieldRequest()); err == nil {
			t.Fatal("alias, grouped, nested, duplicate or foreign declaration accepted")
		}
	}
	r := accessFieldRequest()
	at := strings.Index(accessFieldSource, r.Old)
	for _, offset := range []int{at - 1, at + 1} {
		if err := mutationDeclaration([]byte(accessFieldSource), r, offset); err == nil {
			t.Fatal("adjacent struct mutation offset accepted")
		}
	}
}

func namedCallbackRequest() request {
	return request{File: "fixture_test.go", Function: "named", Literal: "NAMED_CALLBACK_ASSERTION",
		Call: `t.Fatal("NAMED_CALLBACK_ASSERTION")`, ExpectedSites: 1, ExpectedFileSites: 1,
		Chain: []caller{{Caller: "TestRoot", Callee: "named", Kind: "run_callback"}}}
}

const namedCallbackSource = "package fixture\nimport \"testing\"\nfunc TestRoot(t *testing.T) { t.Run(\"child\", named) }\nfunc named(t *testing.T) { t.Fatal(\"NAMED_CALLBACK_ASSERTION\") }\n"

func TestNamedCallbackLineageUsesItsOwnFatal(t *testing.T) {
	result, err := resolve([]byte(namedCallbackSource), namedCallbackRequest())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(result.ReportLines, result.AssertionLines) || result.NamedCallbackSites != 1 || !result.HelperChainValid {
		t.Fatalf("named callback did not keep its own fatal site: %+v", result)
	}
}

func TestNamedCallbackLineageRejectsHelperOrAmbiguousReferences(t *testing.T) {
	cases := []string{
		strings.Replace(namedCallbackSource, "{ t.Fatal", "{ t.Helper(); t.Fatal", 1),
		strings.Replace(namedCallbackSource, `t.Run("child", named)`, `t.Run("child", named); t.Run("second", named)`, 1),
		strings.Replace(namedCallbackSource, `t.Run("child", named)`, `named(t)`, 1),
	}
	for _, source := range cases {
		if _, err := resolve([]byte(source), namedCallbackRequest()); err == nil {
			t.Fatal("unproved callback lineage was accepted")
		}
	}
}

func TestNamedCallbackLineageRejectsWrongReceiverAndMixedEdges(t *testing.T) {
	wrong := strings.Replace(namedCallbackSource, `t.Run("child", named)`, `other.Run("child", named)`, 1)
	if _, err := resolve([]byte(wrong), namedCallbackRequest()); err == nil {
		t.Fatal("a foreign receiver was accepted as the testing root")
	}
	r := namedCallbackRequest()
	r.Chain = append(r.Chain, caller{Caller: "named", Callee: "named", Kind: "helper"})
	if _, err := resolve([]byte(namedCallbackSource), r); err == nil {
		t.Fatal("mixed helper/callback lineage was accepted")
	}
	r = namedCallbackRequest()
	r.CallbackDirect = true
	if _, err := resolve([]byte(namedCallbackSource), r); err == nil {
		t.Fatal("named and anonymous callbacks were conflated")
	}
}
