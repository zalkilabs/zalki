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
