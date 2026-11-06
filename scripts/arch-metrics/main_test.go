package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAnalyzeSource(t *testing.T) {
	const src = `package p

type Store struct{}
type hidden struct{}

const Exported, internalConst = 1, 2

func (s Store) A() {}
func (s *Store) b() {
	_ = 1
}
func (t *hidden) C() {}
func Free() {
	_ = 1
	_ = 2
}
func unexported()
`
	stats, err := analyzeSource("p.go", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	// Store, Exported, (Store).A, hidden.C, Free are exported by name.
	if stats.Exported != 5 {
		t.Errorf("exported = %d, want 5", stats.Exported)
	}
	if stats.StoreMethods != 2 {
		t.Errorf("store methods = %d, want 2 (value and pointer receivers)", stats.StoreMethods)
	}
	spans := map[string]int{}
	for _, fn := range stats.Functions {
		spans[fn.Name] = fn.Lines
	}
	if spans["Free"] != 4 || spans["Store.b"] != 3 {
		t.Errorf("function spans = %v, want Free=4 Store.b=3", spans)
	}
	if _, ok := spans["unexported"]; ok {
		t.Error("a function declaration without a body must not be measured")
	}
	if stats.Lines != 17 {
		t.Errorf("lines = %d, want 17", stats.Lines)
	}
}

func TestAnalyzeSourceRejectsBrokenGo(t *testing.T) {
	if _, err := analyzeSource("bad.go", []byte("package p\nfunc (")); err == nil {
		t.Fatal("a file that does not parse must be an error, not an undercount")
	}
}

func TestRunOnThisModule(t *testing.T) {
	var buf bytes.Buffer
	if err := run(context.Background(), "../..", &buf); err != nil {
		t.Fatalf("the repository must produce a report: %v\n%s", err, buf.String())
	}
	if !strings.Contains(buf.String(), "internal/bus") {
		t.Fatalf("report is missing the core package:\n%s", buf.String())
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestReportSurfacesWriteErrors(t *testing.T) {
	err := writeReport(failingWriter{}, nil, map[string][]string{}, buildInfo{})
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("a failed write must be reported, got %v", err)
	}
}

func TestRunRejectsOtherModule(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{"go.mod": "module example.com/other\n\ngo 1.26\n", "other.go": "package other\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	var buf bytes.Buffer
	err := run(context.Background(), dir, &buf)
	if err == nil || !strings.Contains(err.Error(), "no cmd/ or internal/ packages") {
		t.Fatalf("wrong-module result = %v; output %q", err, buf.String())
	}
}

func TestAnalyzePackageBuildSelectedFiles(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{"plain.go": "package p\nfunc Plain() {}", "cgo.go": "package p\nfunc Cgo() {}\n", "plain_test.go": "package p\n", "external_test.go": "package p_test"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	report, err := analyzePackage(listedPackage{ImportPath: modulePrefix + "internal/p", Dir: dir, GoFiles: []string{"plain.go"}, CgoFiles: []string{"cgo.go"}, TestGoFiles: []string{"plain_test.go"}, XTestGoFiles: []string{"external_test.go"}})
	if err != nil {
		t.Fatal(err)
	}
	if report.ProdFiles != 2 || report.ProdLines != 4 || report.TestLines != 2 || report.Exported != 2 {
		t.Fatalf("unexpected selected-file counts: %+v", report)
	}
}

func TestGenericReceiverNames(t *testing.T) {
	stats, err := analyzeSource("generic.go", []byte("package p; type Store[A,B any] struct{}; func(s *Store[A,B]) Read(){}"))
	if err != nil {
		t.Fatal(err)
	}
	if stats.StoreMethods != 1 || len(stats.Functions) != 1 || stats.Functions[0].Name != "Store.Read" {
		t.Fatalf("generic receiver: %+v", stats)
	}
}

func TestReportLabelsBuildAndScope(t *testing.T) {
	var buf bytes.Buffer
	err := writeReport(&buf, nil, nil, buildInfo{GOOS: "linux", GOARCH: "arm64", GOVERSION: "go1.26.10", CGO_ENABLED: "0"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"linux/arm64, go1.26.10, CGO_ENABLED=0", "fields and interface members are excluded", "other platforms"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("missing scope label %q", want)
		}
	}
}
