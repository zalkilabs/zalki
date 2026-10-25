# Contributing

Use Go 1.26.10 or newer. Work on a short-lived branch and open a focused pull
request describing the problem, resulting behavior, and validation. Squash
merge after CI passes. Maintainers are responsible for reviewing and testing
AI-assisted changes as they would any other contribution.

```sh
make fmt-check test vet lint
```

Use `make fmt` to format code. Add tests for behavior changes and regressions.
Fixtures must use temporary state and configuration directories. Normal tests
must not contact live agent models or modify your real client settings.

For dependency checks:

```sh
go install golang.org/x/vuln/cmd/govulncheck@v1.9.0
make vulncheck
```

Report security issues using [the security policy](SECURITY.md). Remove private
capabilities, message bodies, credentials, and personal paths from reports.

## Client integrations

Client-specific transports, hook mappings, name lookup, and configuration
fragments live in `internal/bus/adapter_<client>.go`. Implement `Adapter`, declare
`ReceiveCapabilities`, and register the adapter in `builtinRegistry`.
`TestNewAdapterUsesCoreWithoutAgentSpecificBranches` exercises the extension path.

Keep identity, authentication, retry, and receipt rules in the shared core.
Client protocol changes usually affect its adapter and fixtures; changes to hook
input or configuration formats can also require shared-helper updates. Verify
both send and receive behavior against the affected client's native protocol.

## Compatibility and cost checks

Client contract fixtures live in `internal/bus/testdata/clients`. Existing fixtures
are normalized observations from earlier disposable-session probes, not raw
captures or a guarantee that current client versions work. `zalki doctor` shows
the installed versions, fixture coverage, listener health, and recovery hints.
Already-running clients may still use an older version until restarted.

To capture a new startup contract, export `ZALKI_CAPTURE_CONTRACTS` as an absolute
private directory before starting the client. Hooks capture allowlisted schema
fields with normalized IDs and paths; prompts, titles, credentials, and tool
inputs are omitted. Review the resulting JSON before adding it to the fixtures.

Normal CI uses fixtures and never invokes live models. For a manual live check,
first load the current Zalki build in fresh disposable client sessions sharing
one workspace and state directory, then run:

```sh
export ZALKI_SMOKE_STATE=/path/to/private/state
export ZALKI_SMOKE_WORKSPACE=/path/to/disposable/workspace
export ZALKI_SMOKE_TARGETS='codex:EXACT_UUID claude:EXACT_UUID cursor:EXACT_UUID'
make smoke-live
```

The runner sends real review requests and records correlated replies, receipts,
versions, and latency in private state reports. It creates and retires its own
receiver identity; it never changes client configuration or permissions.
Fresh-start, idle, and draft checks require operator confirmations. Restart
checks also require a changed registration generation or process-owner fingerprint. Nothing is automatically
resent. To run only the automated round trip, set `ZALKI_SMOKE_CASES=roundtrip`.
Native model usage applies; token counts and internal model turns are reported
as unknown because clients do not expose a common usage interface.

```sh
make cost-check
```

This benchmarks inbox scans versus exact-ID reads at 100, 1,000, and 10,000
records, then measures request/response and tool-schema bytes for a scripted
review/revision exchange. It calls no live models; bytes are a portable cost
proxy, not a token or billing estimate.

`list_messages` returns `data.messages`, `data.more`, and `data.next_cursor`;
continue pages with the same filters and `cursor=next_cursor`. It defaults to five unread
records, and bounds normal pages to 24 KiB. Wake handlers should request the
hint's `message_id` directly, avoiding a journal scan. History is opt-in and omits
bodies unless `include_body=true`; exact reads return complete bodies.
`body_omitted` means fetch by ID; `body_deferred` means await native delivery.

## Releases

Maintainers release reviewed commits from `main` using tags such as `v0.1.0`
or `v0.1.0-rc.1`. Pushing a tag runs CI, builds and checks macOS archives and a
source archive, generates an architecture-specific binary Homebrew cask, and
creates a draft release with release notes. Verify the draft, then publish RC
tags as prereleases without marking them Latest. Public prereleases support
anonymous downloads; drafts and private repositories require GitHub access.
Tagged releases must be signed and
accepted by Apple notarization before packaging. Only the draft-creation job has
repository write access. Local and CI snapshots do not use signing credentials.

Configure these Actions secrets before creating a release tag. Organization
secrets must grant access to this repository:

| Secret | Value |
| --- | --- |
| `MACOS_SIGN_P12` | Base64-encoded Developer ID Application identity, including its private key |
| `MACOS_SIGN_PASSWORD` | Password protecting that identity export |
| `MACOS_NOTARY_KEY` | Base64-encoded App Store Connect Team API private key |
| `MACOS_NOTARY_KEY_ID` | API key ID |
| `MACOS_NOTARY_ISSUER_ID` | API issuer UUID |

Export only the release signing identity from Keychain Access as a
password-protected `.p12`. Enter credentials directly from your own terminal;
do not put private key contents in chat, commits, or command arguments:

```sh
base64 -i /path/to/release-signing.p12 | gh secret set MACOS_SIGN_P12 --repo zalkilabs/zalki
gh secret set MACOS_SIGN_PASSWORD --repo zalkilabs/zalki
base64 -i /path/to/AuthKey_KEYID.p8 | gh secret set MACOS_NOTARY_KEY --repo zalkilabs/zalki
gh secret set MACOS_NOTARY_KEY_ID --repo zalkilabs/zalki
gh secret set MACOS_NOTARY_ISSUER_ID --repo zalkilabs/zalki
```

The commands without piped input prompt for their values. A release fails if
any required secret is missing. The tap's versioned cask installs the signed
binary and bundled completions; manual development builds require Go.

Before publishing the draft, check the assets and release notes and test native
Claude, Codex, and Cursor messaging, idle wake, draft preservation, and
install/upgrade/uninstall on a disposable macOS setup. Record the tested client
versions in the release notes. Automated fixtures do not establish live-client
compatibility. Keep prebuilt macOS signing/notarization limits clear to users.

Local packaging checks require GoReleaser v2.18.3:

```sh
make release-snapshot
```

Source archives include committed files only. `dist/zalki.rb` is the generated
release cask; review it before updating the tap. An existing release is never
overwritten automatically. Delete an incomplete draft before retrying its job.

After the first push, configure GitHub to require the CI checks and pull requests
on `main`, block force pushes/deletion, and protect release tags. Solo maintainers
can merge their own passing PRs. Repository protection and private vulnerability
reporting are GitHub settings, not enabled by adding these files.
