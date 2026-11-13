// Command test-alias runs the full required race suite under an explicit
// symlinked temporary directory. The JSON report outlives its disposable fixture.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	err := run(ctx, os.Stdout, os.Stderr)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, out, diagnostic io.Writer) (err error) {
	repo, err := os.Getwd()
	if err != nil {
		return err
	}
	repo, err = filepath.EvalSymlinks(repo)
	if err != nil {
		return err
	}
	source, count, err := sourceDigest(repo)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, verifyAliasSource(repo, source, count)) }()
	fixture, err := newAliasFixture()
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, fixture.close()) }()
	report, err := newAliasReport(fixture.root)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, report.Close()) }()
	if _, err := fmt.Fprintf(out, "Alias gate: full; source SHA256 %s (%d input files)\nFixture: %s\nCanonical temp: %s\nAlias temp: %s\nGo JSON report: %s\n", source, count, fixture.root, fixture.real, fixture.alias, report.Name()); err != nil {
		return err
	}
	goTool, env, err := buildEnvironment(ctx, repo, fixture, out, diagnostic)
	if err != nil {
		return err
	}
	binary, checker := filepath.Join(fixture.root, "zalki"), filepath.Join(fixture.root, "check-integration")
	for _, target := range []struct{ binary, pkg string }{{binary, "./cmd/zalki"}, {checker, "./scripts/check-integration"}} {
		if err := runChild(ctx, repo, env, out, diagnostic, goTool, "build", "-p=1", "-mod=readonly", "-o", target.binary, target.pkg); err != nil {
			return err
		}
	}
	testEnv := replaceEnvironment(env, map[string]string{"TMPDIR": fixture.alias, "ZALKI_TEST_ALIAS_ROOT": fixture.alias, "ZALKI_TEST_BINARY": binary}, []string{"GOTMPDIR"})
	if _, err := fmt.Fprintf(out, "Test child: TMPDIR=%s; GOTMPDIR absent; ZALKI_TEST_BINARY=%s\n", fixture.alias, binary); err != nil {
		return err
	}
	if err := runChild(ctx, repo, testEnv, report, diagnostic, goTool, "test", "-p=1", "-mod=readonly", "-race", "-parallel=2", "-count=1", "-json", "./..."); err != nil {
		return fmt.Errorf("alias suite failed (JSON retained at %s): %w", report.Name(), err)
	}
	if err := report.Sync(); err != nil {
		return err
	}
	return runChild(ctx, repo, env, out, diagnostic, checker, "--alias", report.Name())
}

func newAliasReport(root string) (*os.File, error) {
	if path := os.Getenv("ZALKI_TEST_ALIAS_REPORT"); path != "" {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		parent, err := filepath.EvalSymlinks(filepath.Dir(absolute))
		if err != nil {
			return nil, err
		}
		absolute = filepath.Join(parent, filepath.Base(absolute))
		relative, err := filepath.Rel(root, absolute)
		if err != nil {
			return nil, err
		}
		if relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return nil, errors.New("alias report must be outside the disposable fixture")
		}
		return os.OpenFile(absolute, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600) //nolint:gosec // G304: explicit new evidence path; O_EXCL refuses overwrite and final symlink.
	}
	return os.CreateTemp(filepath.Dir(root), "za-report-*.json")
}
