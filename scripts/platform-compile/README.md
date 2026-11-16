# Original Linux CGO-disabled compile coverage

The required Ubuntu Lint job runs this compile-only target after native lint
and architecture comparison. Darwin's existing packaging builds are unchanged.
The two profiles preserve the original structure/release checks:

| Target | Commands |
| --- | --- |
| Linux amd64, CGO disabled | Complete current package discovery, `go vet ./...`, `go build ./...`, `go test -c` for every discovered package containing internal or external test files |
| Linux arm64, CGO disabled | `go vet ./cmd/zalki`, `go build ./...` |

Build flags set `-p=1 -mod=readonly`. Build outputs live outside source. The
script never executes a test binary, the built CLI, or a cross-target program.
It does not repeat runtime, alias, strict-lint, metrics or Windows checks.

Discovery refuses nonzero commands, stderr diagnostics, invalid/empty/duplicate
JSON, incomplete packages, dependency errors, mismatched package paths and CGO
files. Every source package directory missing from the wildcard output needs an
explicit target-specific build-constraint witness; a successful partial list
cannot become coverage. Go's testdata, hidden, vendor and nested-module package
walk boundaries remain explicit. There is no historical package-count ceiling.

Each command retains separate raw stdout/stderr and an exit/timeout receipt.
The result binds the actual Git head/tree, source fingerprint before/after,
compiler hash/version, actual profile settings, selected/excluded packages,
and output hashes. CLI and test executables are not uploaded by the workflow;
their recorded hashes do not claim retained executable artifacts. The existing
always-upload Lint artifact retains JSON and raw streams even after failure.
An existing output directory is refused rather than overwritten.

Pure fake-tool fixtures require no Go compiler or native slot:

```sh
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s scripts/platform-compile -p 'test_*.py' -v
```

Those fixtures prove orchestration refusals, dynamic discovery, output binding
and timeout accounting. Actual current-head hosted compiler outputs are still
required to establish the two target profiles. Refactoring/checkpoint, live
client and release workflows keep their separate reviewed scope.
