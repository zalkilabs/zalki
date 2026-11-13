package main

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

const (
	reviewActorA = "claude:11111111-1111-4111-8111-111111111111"
	reviewActorB = "codex:22222222-2222-4222-8222-222222222222"
	reviewActorC = "cursor:33333333-3333-4333-8333-333333333333"
)

// Namespace cases preserve explicit actor identity throughout selection and review.
func TestPluginReviewNumericNameNamespace(t *testing.T) {
	rows := []pluginReviewTarget{{reviewActorA, "other"}, {reviewActorB, "1"}}
	for _, token := range []string{"1", "01", "-1", "1.0", "#01"} {
		if _, err := pickPluginReviewTargets(token, rows); err == nil {
			t.Fatal("U57_NUMERIC_NAMESPACE", token)
		}
	}
	assertReviewPick(t, "#2", rows, []string{reviewActorB})
}

func TestPluginReviewActorIDLabelNamespace(t *testing.T) {
	rows := []pluginReviewTarget{{reviewActorA, reviewActorB}, {reviewActorB, "beta"}}
	if _, err := pickPluginReviewTargets(reviewActorB, rows); err == nil {
		t.Fatal("U58_BARE_ID_IS_NOT_ID_AUTHORITY")
	}
	assertReviewPick(t, "#1", rows, []string{reviewActorA})
	assertReviewPick(t, "@"+reviewActorB, rows, []string{reviewActorB})
	labels, err := reviewTargetLabels([]string{reviewActorA}, rows)
	if err != nil || !strings.Contains(labels, "(@"+reviewActorA+")") {
		t.Fatal("U58_ID_LOOKING_LABEL_RETAINS_ACTUAL_ID", labels, err)
	}
}

func TestPluginReviewTrimmedNameShadow(t *testing.T) {
	rows := []pluginReviewTarget{{reviewActorA, "worker"}, {reviewActorB, " worker "}}
	for _, token := range []string{"worker", " worker "} {
		if _, err := pickPluginReviewTargets(token, rows); err == nil {
			t.Fatal("U59_TRIM_SHADOW", token)
		}
	}
	assertReviewPick(t, "#2", rows, []string{reviewActorB})
}

func TestPluginReviewCommaLabelShadow(t *testing.T) {
	rows := []pluginReviewTarget{{reviewActorA, "a,b"}, {reviewActorB, "a"}, {reviewActorC, "b"}}
	for _, token := range []string{"a,b", "a, b", "#1,a,b"} {
		if _, err := pickPluginReviewTargets(token, rows); err == nil {
			t.Fatal("U60_COMMA_SHADOW", token)
		}
	}
	assertReviewPick(t, "#1", rows, []string{reviewActorA})
	assertReviewPick(t, "#2,#3", rows, []string{reviewActorB, reviewActorC})
}

func TestPluginReviewReservedNameNamespace(t *testing.T) {
	rows := []pluginReviewTarget{{reviewActorA, "all"}, {reviewActorB, "*"}}
	for _, token := range []string{"all", "ALL", "*"} {
		if _, err := pickPluginReviewTargets(token, rows); err == nil {
			t.Fatal("U61_NO_WILDCARD_NAMESPACE", token)
		}
	}
	assertReviewPick(t, "#1", rows, []string{reviewActorA})
}

func TestPluginReviewSanitizedLabelCollision(t *testing.T) {
	rows := []pluginReviewTarget{{reviewActorA, "\xff"}, {reviewActorB, "\ufffd"}}
	index, err := indexPluginReviewCandidates(rows)
	if err != nil || index.rows[0].label != index.rows[1].label {
		t.Fatal("sanitized collision fixture", err)
	}
	labels, err := reviewTargetLabels([]string{reviewActorA}, rows)
	if err != nil || !strings.Contains(labels, "(@"+reviewActorA+")") {
		t.Fatal("U62_SANITIZED_COLLISION_KEEPS_ID", labels, err)
	}
}

func TestPluginReviewTruncatedLabelCollision(t *testing.T) {
	prefix := strings.Repeat("🚀", 7)
	rows := []pluginReviewTarget{{reviewActorA, prefix + "😀"}, {reviewActorB, prefix + "😁"}}
	index, err := indexPluginReviewCandidates(rows)
	if err != nil || index.rows[0].label != index.rows[1].label || len(index.rows[0].label) > pluginReviewLabelLimit {
		t.Fatal("truncated collision fixture", err)
	}
	labels, err := reviewTargetLabels([]string{reviewActorB}, rows)
	if err != nil || !strings.Contains(labels, "(@"+reviewActorB+")") {
		t.Fatal("U63_TRUNCATED_COLLISION_KEEPS_ID", labels, err)
	}
}

func TestPluginReviewSummaryUsesAllCandidates(t *testing.T) {
	rows := make([]pluginReviewTarget, 64)
	for i := range rows {
		rows[i] = pluginReviewTarget{fmt.Sprintf("codex:%08x-0000-4000-8000-%012x", i+1, i+1), fmt.Sprintf("worker%02d", i+1)}
	}
	rows[0] = pluginReviewTarget{reviewActorA, "worker"}
	rows[63] = pluginReviewTarget{reviewActorB, "worker"}
	if _, err := pickPluginReviewTargets("worker", rows); err == nil {
		t.Fatal("U13_DUPLICATE_NAME_REJECTED")
	}
	p := pluginReviewPlan{
		name: "demo", id: "local/demo", version: "0.1.0", source: "/source/demo", home: "/home",
		run: []string{"plugin"}, access: pluginReviewAccess{status: true, send: true},
		send: []string{reviewActorA}, candidates: rows,
	}
	lines, err := renderPluginReview(p)
	if err != nil || !strings.Contains(strings.Join(lines, "\n"), "\"worker\" (@"+reviewActorA+")") {
		t.Fatal("U64_UNSELECTED_DUPLICATE_KEEPS_ID", lines, err)
	}
	picker, err := renderPluginReviewCandidates(rows)
	if err != nil || !strings.Contains(picker[0], "#1 \"worker\" (@"+reviewActorA+")") || !strings.Contains(picker[63], reviewActorB) {
		t.Fatal("U64_ROWS_SHOW_QUOTED_LABEL_AND_ID", picker, err)
	}
}

func TestPluginReviewSimpleNamesAndBlankRemainUsable(t *testing.T) {
	rows := []pluginReviewTarget{{reviewActorA, "zlaki"}, {reviewActorB, "worker-3"}}
	assertReviewPick(t, "zlaki,worker-3", rows, []string{reviewActorA, reviewActorB})
	assertReviewPick(t, "", rows, []string{})
}

func assertReviewPick(t *testing.T, token string, rows []pluginReviewTarget, want []string) {
	t.Helper()
	got, err := pickPluginReviewTargets(token, rows)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("pick %q: got %v err %v, want %v", token, got, err, want)
	}
}
