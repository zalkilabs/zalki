package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

type syntheticCase struct {
	Identity request `json:"allowed_source_sites_and_caller_mapping"`
	Context  struct {
		Phases map[string]struct {
			Files map[string]string `json:"source_files"`
		} `json:"phases"`
	} `json:"context"`
	Positions struct {
		Reported int `json:"synthetic_reported_line"`
	} `json:"expected_event_assertion_positions"`
}

func loadSynthetic(t *testing.T, name string) syntheticCase {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "fault-controls", "fixtures", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var c syntheticCase
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestAssertionSitesResolveCurrentHelperAndDescendantSource(t *testing.T) {
	for _, name := range []string{"direct-named-assertion-positive", "helper-two-level-caller-positive", "named-descendant-group-positive", "two-rpc-assertion-sites-positive", "helper-source-lines-shift-positive"} {
		t.Run(name, func(t *testing.T) {
			c := loadSynthetic(t, name)
			raw := []byte(c.Context.Phases["intended"].Files[c.Identity.File])
			result, err := resolve(raw, c.Identity)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(result.ReportLines, c.Positions.Reported) || !result.HelperChainValid || result.AssertionSites != c.Identity.ExpectedSites {
				t.Fatalf("current source caller attribution differs: %+v", result)
			}
		})
	}
}

func TestAssertionSitesRejectExtraRPCBranchAndMissingHelper(t *testing.T) {
	for _, name := range []string{"extra-third-rpc-marker-site", "helper-marker-removed"} {
		t.Run(name, func(t *testing.T) {
			c := loadSynthetic(t, name)
			raw := []byte(c.Context.Phases["intended"].Files[c.Identity.File])
			_, err := resolve(raw, c.Identity)
			if err == nil {
				t.Fatal("unreviewed assertion site/helper change was accepted")
			}
			expected := "site inventory"
			if name == "helper-marker-removed" {
				expected = "caller/helper chain"
			}
			if !strings.Contains(err.Error(), expected) {
				t.Fatalf("wrong refusal: %v", err)
			}
		})
	}
}

func TestAssertionSitesRefuseDisconnectedOrAmbiguousCallers(t *testing.T) {
	c := loadSynthetic(t, "helper-two-level-caller-positive")
	raw := []byte(c.Context.Phases["good"].Files[c.Identity.File])
	c.Identity.Chain[0].Caller = "checkSentinel"
	if _, err := resolve(raw, c.Identity); err == nil {
		t.Fatal("disconnected caller chain was accepted")
	}
	c = loadSynthetic(t, "helper-two-level-caller-positive")
	raw = []byte(strings.Replace(string(raw), "  checkRoute(t)", "  checkRoute(t); checkRoute(t)", 1))
	if _, err := resolve(raw, c.Identity); err == nil {
		t.Fatal("ambiguous helper callers were accepted")
	}
}

func TestAssertionSitesRejectSourceLinksAndMetadata(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	external := filepath.Join(outside, "external_test.go")
	if err := os.WriteFile(external, []byte("package external\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(root, "linked_test.go")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "directory")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"linked_test.go", "directory/external_test.go", "../external_test.go", ".git/config"} {
		if _, err := rootedSource(root, name); err == nil {
			t.Fatalf("unsupported assertion source accepted: %s", name)
		}
	}
}

func TestAssertionSitesBindTheWholeQuotedFatalCall(t *testing.T) {
	c := loadSynthetic(t, "direct-named-assertion-positive")
	raw := c.Context.Phases["good"].Files[c.Identity.File]
	changed := strings.Replace(raw, `t.Fatal("SYNTHETIC_GUARD_FAILED")`, `t.Fatal("SYNTHETIC_GUARD_FAILED", "different predicate evidence")`, 1)
	if _, err := resolve([]byte(changed), c.Identity); err == nil {
		t.Fatal("changed fatal arguments reused an old assertion identity")
	}
}

func TestAssertionSitesFreshTestingCallbackReportsItsOwnFatal(t *testing.T) {
	const source = "package fixture\nimport \"testing\"\nfunc TestRoot(t *testing.T) { check(t) }\nfunc check(t *testing.T) {\n t.Helper()\n t.Run(\"child\",func(t *testing.T) {\n  t.Fatal(\"CURRENT_CALLBACK_ASSERTION\")\n })\n}\n"
	r := request{File: "fixture_test.go", Function: "check", Literal: "CURRENT_CALLBACK_ASSERTION", Call: `t.Fatal("CURRENT_CALLBACK_ASSERTION")`, ExpectedSites: 1, ExpectedFileSites: 1, RequireHelpers: true, CallbackDirect: true, Chain: []caller{{Caller: "TestRoot", Callee: "check"}}}
	result, err := resolve([]byte(source), r)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.Count(source[:strings.Index(source, `t.Fatal("CURRENT_CALLBACK_ASSERTION")`)], "\n") + 1
	if !slices.Equal(result.ReportLines, []int{line}) || result.CallbackSites != 1 {
		t.Fatalf("fresh callback inherited its parent's helper attribution: %+v", result)
	}
	c := loadSynthetic(t, "helper-two-level-caller-positive")
	c.Identity.CallbackDirect = true
	if _, err := resolve([]byte(c.Context.Phases["good"].Files[c.Identity.File]), c.Identity); err == nil {
		t.Fatal("ordinary helper falsely claimed direct fresh-callback attribution")
	}
}

func TestAssertionMutationPreimageStaysInsideItsNamedFunction(t *testing.T) {
	const source = "package fixture\nfunc other() bool { return false }\nfunc reviewed() bool { return true }\n"
	r := mutationRequest{File: "source.go", Symbol: "reviewed", Old: "return false", Count: 1}
	if _, err := bindMutation([]byte(source), r); err == nil {
		t.Fatal("same preimage in an unrelated function was accepted")
	}
	r.Symbol = "other"
	result, err := bindMutation([]byte(source), r)
	if err != nil || result.Offset != strings.Index(source, r.Old) {
		t.Fatalf("exact named mutation preimage was not bound: %+v, %v", result, err)
	}
}
