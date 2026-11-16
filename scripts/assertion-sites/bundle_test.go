package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func bundleSource(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(name)))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestBundleCurrentAssertionsBindReviewedWholeRootSites(t *testing.T) {
	var controls []struct {
		ID           string    `json:"id"`
		Assertion    request   `json:"assertion"`
		Secondary    []request `json:"secondary_assertions"`
		Observations []struct {
			Assertion request `json:"assertion"`
		} `json:"observations"`
	}
	raw, err := os.ReadFile(filepath.Join("..", "fault-controls", "controls.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &controls); err != nil {
		t.Fatal(err)
	}
	examined := 0
	for _, control := range controls {
		if !slices.Contains([]string{"Z71", "Z74", "Z70", "Z66", "Z67", "Z68"}, control.ID[:min(3, len(control.ID))]) && control.ID != "M1F1" {
			continue
		}
		identities := append([]request{control.Assertion}, control.Secondary...)
		for _, observation := range control.Observations {
			identities = append(identities, observation.Assertion)
		}
		for _, identity := range identities {
			result, err := resolve(bundleSource(t, identity.File), identity)
			if err != nil {
				t.Fatalf("%s/%s/%s: %v", control.ID, identity.Function, identity.Literal, err)
			}
			if !slices.EqualFunc(result.Chain, identity.Chain, func(a, b caller) bool { return a == b }) {
				t.Fatalf("%s current source chain echo differs", control.ID)
			}
		}
		examined++
	}
	if examined != 27 {
		t.Fatalf("bundle26 plus original M1F1 inventory differs: %d", examined)
	}
}

func TestBundleLiteralArgumentsKeepDistinctSiblingReports(t *testing.T) {
	raw := bundleSource(t, loudStopFile)
	reports := map[int]bool{}
	for _, callback := range []string{"signal death", "ignored stop"} {
		r := closedLoudRequest(callback)
		result, err := resolve(raw, r)
		if err != nil {
			t.Fatal(err)
		}
		b := result.LiteralBinding
		if b == nil || b.PhysicalFatalSites != 1 || b.InstantiatedSites != 1 || b.SiblingSites != 2 || b.Request != *r.LiteralArgument {
			t.Fatalf("physical fatal and instantiated binding conflated: %+v", result)
		}
		if !slices.Equal(result.AssertionLines, []int{61}) || len(result.ReportLines) != 1 {
			t.Fatalf("source fatal/caller interval differs: %+v", result)
		}
		reports[result.ReportLines[0]] = true
	}
	if !reports[22] || !reports[25] || len(reports) != 2 {
		t.Fatalf("sibling caller reports conflated: %+v", reports)
	}
}

func TestBundleLiteralArgumentsRejectWrongRequestBindings(t *testing.T) {
	edits := []func(*request){
		func(r *request) { r.Literal = "wrong literal" },
		func(r *request) { r.LiteralArgument.ArgumentIndex = 2 },
		func(r *request) { r.LiteralArgument.Parameter = "mode" },
		func(r *request) { r.LiteralArgument.ParameterType = "any" },
		func(r *request) { r.LiteralArgument.TestArgument = "other" },
		func(r *request) { r.Function = "checkQuietDisable" },
		func(r *request) { r.LiteralArgument.Callback = "signal death" },
		func(r *request) { r.LiteralArgument.Root = "TestOther" },
		func(r *request) { r.LiteralArgument.ExpectedCalls = 2 },
		func(r *request) { r.Chain[0].ExpectedCalls = 1 },
		func(r *request) { r.ExpectedFileSites = 2 },
		func(r *request) { r.Call = "other.Fatal(failure)" },
	}
	raw := bundleSource(t, loudStopFile)
	for index, edit := range edits {
		r := closedLoudRequest("ignored stop")
		edit(&r)
		if _, err := resolve(raw, r); err == nil {
			t.Fatalf("wrong literal request %d was accepted", index)
		}
	}
}

func TestBundleLiteralArgumentsRejectChangedSourceBoundaries(t *testing.T) {
	raw := string(bundleSource(t, loudStopFile))
	cases := [][2]string{
		{"failure string)", "failure any)"},
		{"t.Fatal(failure)", "other.Fatal(failure)"},
		{"t.Fatal(failure)", "failure = \"alias\"; t.Fatal(failure)"},
		{"t.Fatal(failure)", "{ failure := \"shadow\"; t.Fatal(failure) }"},
		{"checkLoudStop(t, \"ignore-on-code\", false,", "checkLoudStop(other, \"ignore-on-code\", false,"},
		{"t.Run(\"ignored stop\"", "other.Run(\"ignored stop\""},
		{"t.Run(\"ignored stop\"", "t.Run(\"unregistered\""},
		{`"a plugin that ignored the stop was reported as clean")`, `failure)`},
		{`"a plugin that ignored the stop was reported as clean")`, `"a plugin that died by a signal at disable was reported as clean")`},
		{`"a plugin that ignored the stop was reported as clean")`, `"a plugin that ignored the stop was reported as clean"...)`},
		{`"a plugin that ignored the stop was reported as clean")`, `"a plugin that ignored the stop was reported as clean"); checkLoudStop(t,"ignore-on-code",false,"a plugin that ignored the stop was reported as clean")`},
		{"func checkLoudStop(t *testing.T, mode string, receive bool, failure string) {\n\tt.Helper()", "func checkLoudStop(t *testing.T, mode string, receive bool, failure string) {\n\tt.Helper(); t := other"},
	}
	for index, pair := range cases {
		if !strings.Contains(raw, pair[0]) {
			t.Fatalf("negative fixture %d has no current preimage", index)
		}
		changed := strings.Replace(raw, pair[0], pair[1], 1)
		if _, err := resolve([]byte(changed), closedLoudRequest("ignored stop")); err == nil {
			t.Fatalf("changed literal boundary %d was accepted", index)
		}
	}
}

func TestBundleRegisteredPrefixesRejectWrongOrDisconnectedPaths(t *testing.T) {
	const source = `package fixture
import "testing"
func TestRoot(t *testing.T) { t.Run("first", first) }
func first(t *testing.T) { t.Run("second", second) }
func second(t *testing.T) { t.Fatal("REVIEWED") }
`
	r := request{File: "fixture_test.go", Function: "second", Literal: "REVIEWED", Call: `t.Fatal("REVIEWED")`, ExpectedSites: 1, ExpectedFileSites: 1,
		Chain: []caller{{Kind: "run_callback", Caller: "TestRoot", Callee: "first"}, {Kind: "run_callback", Caller: "first", Callee: "second"}}}
	result, err := resolve([]byte(source), r)
	if err != nil || result.NamedCallbackSites != 2 {
		t.Fatalf("registered prefix: %+v %v", result, err)
	}
	wrong := strings.Replace(source, `t.Run("second", second)`, `other.Run("second", second)`, 1)
	if _, err := resolve([]byte(wrong), r); err == nil {
		t.Fatal("foreign nested registration accepted")
	}
	r.Chain[1].Caller = "TestRoot"
	if _, err := resolve([]byte(source), r); err == nil {
		t.Fatal("disconnected registered prefix accepted")
	}
	const inner = `package fixture
import "testing"
func TestRoot(t *testing.T) { t.Run("first", first) }
func first(t *testing.T) { t.Helper(); invoke(func(t *testing.T) { t.Fatal("REVIEWED") }) }
`
	fresh := request{File: "fixture_test.go", Function: "first", Literal: "REVIEWED", Call: `t.Fatal("REVIEWED")`, ExpectedSites: 1, ExpectedFileSites: 1,
		CallbackDirect: true, Chain: []caller{{Kind: "run_callback", Caller: "TestRoot", Callee: "first"}}}
	if _, err := resolve([]byte(inner), fresh); err == nil {
		t.Fatal("unregistered anonymous callback accepted")
	}
	registered := strings.Replace(inner, "invoke(func", `t.Run("child",func`, 1)
	if _, err := resolve([]byte(registered), fresh); err != nil {
		t.Fatalf("registered fresh callback refused: %v", err)
	}

	captured := strings.Replace(registered, "func(t *testing.T)", "func(other *testing.T)", 1)
	if captured == registered {
		t.Fatal("receiver-negative fixture has no current callback parameter")
	}
	if _, err := resolve([]byte(captured), fresh); err == nil {
		t.Fatal("captured parent testing receiver accepted as fresh callback receiver")
	}
	mixed := strings.Replace(captured, `t.Fatal("REVIEWED")`, `other.Fatal("REVIEWED"); t.Fatal("REVIEWED")`, 1)
	if _, err := resolve([]byte(mixed), fresh); err == nil {
		t.Fatal("mixed parent and callback testing receivers accepted")
	}

}
