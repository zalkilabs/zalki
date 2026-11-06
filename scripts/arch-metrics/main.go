// Command arch-metrics prints a structural health report for periodic
// architecture checkpoints: package sizes, API surface, the longest functions,
// and the internal import graph for the selected Go build.
//
// This command reports measurements only. Boundary enforcement belongs to
// depguard in .golangci.yml; make arch-metrics runs that check separately.
// Cognitive complexity is reported by make lint-strict.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
)

const modulePrefix = "github.com/zalkilabs/zalki/"

// longFunctionLines is the length above which a function is reported.
const longFunctionLines = 60

type listedPackage struct {
	ImportPath   string
	Dir          string
	GoFiles      []string
	CgoFiles     []string
	TestGoFiles  []string
	XTestGoFiles []string
	Imports      []string
}

// fileStats is what one Go file contributes to the report.
type fileStats struct {
	Lines        int
	Exported     int // exported top-level funcs, methods, types, consts and vars
	StoreMethods int // methods whose receiver is Store or *Store
	Functions    []functionSpan
}

type functionSpan struct {
	Name  string
	Lines int
}

// analyzeSource parses one Go source file. It returns an error for source that
// does not parse so a broken tree is reported instead of silently undercounted.
func analyzeSource(name string, src []byte) (fileStats, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
	if err != nil {
		return fileStats{}, err
	}
	stats := fileStats{Lines: countLines(src)}
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			stats.addFunc(fset, d)
		case *ast.GenDecl:
			stats.addGen(d)
		}
	}
	return stats, nil
}

func (s *fileStats) addFunc(fset *token.FileSet, d *ast.FuncDecl) {
	label := d.Name.Name
	if d.Recv != nil && len(d.Recv.List) == 1 {
		recv := receiverName(d.Recv.List[0].Type)
		label = recv + "." + d.Name.Name
		if recv == "Store" {
			s.StoreMethods++
		}
	}
	if d.Name.IsExported() {
		s.Exported++
	}
	if d.Body != nil {
		lines := fset.Position(d.End()).Line - fset.Position(d.Pos()).Line + 1
		s.Functions = append(s.Functions, functionSpan{Name: label, Lines: lines})
	}
}

func (s *fileStats) addGen(d *ast.GenDecl) {
	for _, spec := range d.Specs {
		switch sp := spec.(type) {
		case *ast.TypeSpec:
			if sp.Name.IsExported() {
				s.Exported++
			}
		case *ast.ValueSpec:
			for _, n := range sp.Names {
				if n.IsExported() {
					s.Exported++
				}
			}
		}
	}
}

func receiverName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return receiverName(t.X)
	case *ast.IndexExpr:
		return receiverName(t.X)
	case *ast.IndexListExpr:
		return receiverName(t.X)
	case *ast.Ident:
		return t.Name
	}
	return "?"
}

type packageReport struct {
	Path                                     string
	ProdFiles, ProdLines, TestLines          int
	Exported, StoreMethods, StoreMethodFiles int
	Long                                     []functionSpan
}

func listPackages(ctx context.Context, dir string) ([]listedPackage, error) {
	cmd := exec.CommandContext(ctx, "go", "list", "-json", "./...")
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	var pkgs []listedPackage
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var p listedPackage
		if err := dec.Decode(&p); err == io.EOF {
			break
		} else if err != nil {
			return nil, err
		}
		pkgs = append(pkgs, p)
	}
	return pkgs, nil
}

func relative(importPath string) string { return strings.TrimPrefix(importPath, modulePrefix) }

func analyzePackage(p listedPackage) (packageReport, error) {
	r := packageReport{Path: relative(p.ImportPath)}
	for _, name := range append(append([]string{}, p.GoFiles...), p.CgoFiles...) {
		src, err := os.ReadFile(filepath.Join(p.Dir, name)) //nolint:gosec // G304: files listed by go list under the module
		if err != nil {
			return r, err
		}
		stats, err := analyzeSource(name, src)
		if err != nil {
			return r, err
		}
		r.ProdFiles++
		r.ProdLines += stats.Lines
		r.Exported += stats.Exported
		if stats.StoreMethods > 0 {
			r.StoreMethods += stats.StoreMethods
			r.StoreMethodFiles++
		}
		for _, fn := range stats.Functions {
			if fn.Lines > longFunctionLines {
				r.Long = append(r.Long, fn)
			}
		}
	}
	for _, name := range append(append([]string{}, p.TestGoFiles...), p.XTestGoFiles...) {
		src, err := os.ReadFile(filepath.Join(p.Dir, name)) //nolint:gosec // G304: files listed by go list under the module
		if err != nil {
			return r, err
		}
		r.TestLines += countLines(src)
	}
	sort.Slice(r.Long, func(i, j int) bool {
		if r.Long[i].Lines == r.Long[j].Lines {
			return r.Long[i].Name < r.Long[j].Name
		}
		return r.Long[i].Lines > r.Long[j].Lines
	})
	return r, nil
}

// printer remembers the first write error so report code stays readable.
type printer struct {
	w   io.Writer
	err error
}

func (p *printer) printf(format string, args ...any) {
	if p.err == nil {
		_, p.err = fmt.Fprintf(p.w, format, args...)
	}
}

func (p *printer) println(text string) { p.printf("%s\n", text) }

type buildInfo struct {
	GOOS        string
	GOARCH      string
	GOVERSION   string
	CGO_ENABLED string // Go environment key.
}

func readBuildInfo(ctx context.Context, dir string) (buildInfo, error) {
	cmd := exec.CommandContext(ctx, "go", "env", "-json", "GOOS", "GOARCH", "GOVERSION", "CGO_ENABLED")
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return buildInfo{}, fmt.Errorf("go env: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	var info buildInfo
	err = json.Unmarshal(out, &info)
	return info, err
}

func countLines(src []byte) int {
	n := bytes.Count(src, []byte("\n"))
	if len(src) > 0 && src[len(src)-1] != '\n' {
		n++
	}
	return n
}

func writeReport(w io.Writer, reports []packageReport, edges map[string][]string, info buildInfo) error {
	p := &printer{w: w}
	p.printf("Build: %s/%s, %s, CGO_ENABLED=%s.\n\n", info.GOOS, info.GOARCH, info.GOVERSION, info.CGO_ENABLED)
	p.println("Counts include build-selected Go and cgo source files under cmd/ and internal/; other platforms, ignored files and scripts/ are excluded.")
	p.println("Exported declarations count top-level functions, methods, types, constants and variables by name; fields and interface members are excluded.")
	p.println("Function lengths count physical lines from declaration through closing brace.\n")
	p.println("## Packages")
	p.println("")
	p.println("| Package | Files | Source lines | Test lines | Exported decls | Store methods (files) |")
	p.println("| --- | ---: | ---: | ---: | ---: | ---: |")
	for _, r := range reports {
		store := ""
		if r.StoreMethods > 0 {
			store = fmt.Sprintf("%d (%d)", r.StoreMethods, r.StoreMethodFiles)
		}
		p.printf("| %s | %d | %d | %d | %d | %s |\n", r.Path, r.ProdFiles, r.ProdLines, r.TestLines, r.Exported, store)
	}
	p.printf("\n## Functions over %d lines\n\n", longFunctionLines)
	found := false
	for _, r := range reports {
		for _, fn := range r.Long {
			found = true
			p.printf("- %s: %s (%d lines)\n", r.Path, fn.Name, fn.Lines)
		}
	}
	if !found {
		p.println("None.")
	}
	p.println("\n## Internal imports")
	p.println("")
	names := make([]string, 0, len(edges))
	for from := range edges {
		names = append(names, from)
	}
	sort.Strings(names)
	for _, from := range names {
		p.printf("- %s -> %s\n", from, strings.Join(edges[from], ", "))
	}
	return p.err
}

func run(ctx context.Context, dir string, w io.Writer) error {
	pkgs, err := listPackages(ctx, dir)
	if err != nil {
		return err
	}
	var reports []packageReport
	edges := map[string][]string{}
	for _, p := range pkgs {
		if !strings.HasPrefix(p.ImportPath, modulePrefix) {
			continue
		}
		rel := relative(p.ImportPath)
		if !strings.HasPrefix(rel, "internal/") && !strings.HasPrefix(rel, "cmd/") {
			continue
		}
		r, err := analyzePackage(p)
		if err != nil {
			return fmt.Errorf("%s: %w", rel, err)
		}
		reports = append(reports, r)
		var targets []string
		for _, imp := range p.Imports {
			if strings.HasPrefix(imp, modulePrefix) {
				targets = append(targets, relative(imp))
			}
		}
		sort.Strings(targets)
		edges[rel] = targets
	}
	if len(reports) == 0 {
		return fmt.Errorf("no cmd/ or internal/ packages in module %s", strings.TrimSuffix(modulePrefix, "/"))
	}
	sort.Slice(reports, func(i, j int) bool {
		if reports[i].ProdLines == reports[j].ProdLines {
			return reports[i].Path < reports[j].Path
		}
		return reports[i].ProdLines > reports[j].ProdLines
	})
	info, err := readBuildInfo(ctx, dir)
	if err != nil {
		return err
	}
	return writeReport(w, reports, edges, info)
}

func main() {
	dir := flag.String("C", ".", "module root directory")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	err := run(ctx, *dir, os.Stdout)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "arch-metrics:", err)
		os.Exit(1)
	}
}
