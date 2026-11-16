# On-demand architecture checkpoint

The manual `CI` workflow captures this report once on its native macOS Lint
runner, after the existing analysis steps have joined. Push and pull-request
runs do not run coverage. The ordinary, race, alias and full unraced commands
remain separate and unchanged.

Dispatch the existing `ci.yml` on the reviewed branch with `gh workflow run
ci.yml --ref BRANCH`. Bind the returned run/attempt and actual checkout SHA/tree
to the intended published head before accepting its evidence. Dispatch runs the
whole existing workflow, not just the checkpoint step. A rerun retains its
original event/source and cannot add capture steps to an old commit. The workflow
already exists on the default branch, so this branch dispatch needs no preliminary
foundation merge. A new reviewed commit containing these steps is required.

The capture reuses the versioned native AST tool's human output: package file
counts, physical source/test lines, exported declarations, Store breadth,
functions over 60 physical lines, and the selected internal graph. Existing
normal/strict/depguard analysis remains authoritative; this adds no tolerance.

Coverage is a distinct `go test -cover -count=1 -p=1 -json -timeout=10m
-coverprofile=... ./...` measurement with native CGO selection, a clean isolated
source clone, private HOME/client directories/TMP, live smoke disabled, and a
fresh nonrace CLI with exact source/SDK build metadata. No percentage threshold
or cross-target runtime claim is introduced. Complete package/file discovery,
target exclusions, required root/descendant results, full stdout/stderr, durable
exit/timeout receipts, SDK/tool/CLI hashes, raw coverage and before/after source
fingerprints are retained. A partial discovery, failure, required skip, missing
profile or changed source refuses success. CLI executables are not uploaded.

The isolated hosted runner supplies quiet-machine context; it is not evidence
that a user's machine is quiet. The command retains its own elapsed time, not a
forecast of whole-job margin. A job-level cancellation can leave only partial
artifacts and is not a completed capture. Human interpretation, scaffold/docs
review, explicit decisions and the owner's checkpoint summary remain separate.
