# Source-bound focused CI controls

`controls.json` is one reviewed recipe list consumed by one runner and one
assertion oracle. The original recipes retain their default raced test build,
45-second test deadline and 120-second owned-process deadline. No recipe may
supply arbitrary commands, environment overrides or timeout values.

The closed `r7_background` profile uses the reviewed non-race build, 120-second
test deadline, 150-second owned-process deadline and 240-second build deadline.
Broad package race coverage remains in the existing native alias jobs. Every R7
phase receives an actual source-bound CLI. The baseline CLI is read back before
and after each use; `rebuild_cli_for_mutant` additionally builds the real CLI
from the attested intended mutation. M1F1 requires that rebuild. The shared CLI
helper also serves full unraced package coverage: raw build metadata, exact
revision, clean or explicitly attested intended state, native identity and
non-race settings are checked. Omitted empty Go setting values decode as empty
strings; missing VCS evidence never passes. CLI builders use isolated local
clones with real Git directories, not linked worktree metadata.

A production mutation binds an exact preimage to one named function body or an
explicit `const`/`var` ValueSpec. Package declaration bindings cannot cross
adjacent declarations or borrow an unrelated name. A singular declaration may
include its keyword, and an owned trailing comment remains within its source
scope. Source links and metadata paths remain unsupported.

Assertions bind the complete current Fatal call and literal, not a stored line
number. Existing helper chains and fresh anonymous callbacks retain their
rules. A `run_callback` edge binds one named, non-helper testing callback to one
current selected-root `t.Run` reference and reports its own fatal location.

Every stream must establish unique RUN/terminal states, one exact package
outcome and output inside the emitting node's actual interval. Mutant failures
must be registered by the positive run. The R7 profile additionally applies the
reviewed functional-site rule: the exact primary named assertion remains
mandatory, while sibling failures may be explained only by current AST-bound
Fatal sites of that same root. This includes formatted values at the same
primary Fatalf call. Secondary sites never count as additional controls.
Compiler/setup/panic/race/timeout, foreign/sibling, skipped, unfinished or
unexplained failures cannot earn a kill. The default recipes do not opt into
this additional functional-site policy.

`native_os` declares applicability, not optional successful execution. Darwin-
only L1F1 stays in the plan on Linux with an explicit non-applicable record and
no execution credit; it must really compile and execute on Darwin. Result
accounting separates cases, intended selections, unique source mutants, test
binary hashes and combined CLI/test execution identities. FD5 remains outside
the original feature table and is not duplicated.

Retain actual build, raw metadata, source fingerprints, binary hashes, selected
streams and restoration receipts. Pure fake-tool tests are not compiled Go or
native mutation evidence. New actual-head native jobs must prove the profiles,
all applicable recipes, full selected unraced packages, current required roots,
strict and architecture comparisons and measured timeout margins before any
local gate is retired.
