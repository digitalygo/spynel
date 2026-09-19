---
status: completed
created_at: 2026-09-19
files_edited:
  - .github/workflows/AGENTS.md
  - .github/workflows/release.yml
  - .gitignore
  - AGENTS.md
  - README.md
  - cmd/spynel/main.go
  - docs/getting-started.md
  - docs/releasing.md
  - go.mod
  - install.sh
  - internal/**/*.go
  - npm/AGENTS.md
  - npm/install.js
  - npm/prepare-release.js
  - npm/test.js
  - package.json
  - scripts/dev_test.go
  - scripts/native-evidence/main.go
  - scripts/test-standalone.py
  - uninstall.sh
rationale: Migrate Spynel's canonical module, release, installer, updater, and documentation coordinates to the Digitalygo repository without changing supported distribution formats.
supporting_docs:
  - ../research/2026-09-19-spynel-architecture-security-quality-gates.md
  - ../../../docs/releasing.md
---

# Digitalygo repository migration

## Summary of changes

Spynel now treats `github.com/digitalygo/spynel` as its canonical repository and Go module. Runtime update discovery, native downloads, npm metadata, release automation, public install commands, tests, documentation, and DOX contracts use the Digitalygo repository.

Intentional Agent Zero ecosystem links and the Agent Zero CLI connector remain unchanged. The npm package name remains `spynel`, and the supported release outputs remain the existing Linux and macOS native archives plus npm.

## Technical reasoning

The fork already used `https://github.com/digitalygo/spynel` as its Git remote, but source imports, updater defaults, installers, npm release preparation, workflow fixtures, and documentation still targeted upstream. A partial replacement would have built Digitalygo code while later downloading updates or native npm assets from upstream.

The migration therefore changed the repository identity as one coordinated contract:

- the Go module and all first-party imports use `github.com/digitalygo/spynel`;
- updater, shell installer, npm installer, package metadata, and generated README links use Digitalygo release coordinates;
- public install and uninstall commands fetch the committed scripts from the Digitalygo default branch;
- future release tests use Digitalygo-owned baseline artifacts;
- a deterministic regression test rejects retired first-party coordinates.

The first Digitalygo release requires a bootstrap exception because no prior Digitalygo release asset exists. For `v1.0.0`, each native runner builds a synthetic `v0.99.0` archive from the same source and exercises the complete standalone install, update, restart, and uninstall path with explicit modern-contract assertions. Later releases download and checksum-verify the published Digitalygo `v1.0.0` baseline and use the same modern-contract mode. Legacy predecessor archives retain a separate historical assertion mode.

A manual workflow dispatch was confirmed to validate and build only. It cannot enter the publication job. Publishing a GitHub Release with a `v`-prefixed tag remains the action that starts asset and npm publication.

## Impact assessment

- Go import identity now matches the repository origin.
- Standalone and npm installations obtain native assets from Digitalygo releases.
- Runtime update checks query Digitalygo GitHub releases.
- npm provenance documentation names the Digitalygo owner and repository.
- The first `v1.0.0` release no longer depends on a nonexistent prior Digitalygo asset or an upstream executable fixture.
- Releases after `v1.0.0` require the four native `v1.0.0` archives and `checksums.txt` to remain available.
- Existing Agent Zero community and connector relationships are preserved.

The version `v1.0.0` was not published by the manual workflow run observed during this operation. After these changes reach the intended commit, an operator must publish the GitHub Release to enter the workflow's publication job.

## Validation steps

The final implementation passed:

- `scripts/dev.sh dox`;
- `go test ./...`;
- `go vet ./...`;
- `go build -o .tmp-bin/spynel ./cmd/spynel`;
- `scripts/smoke.sh`;
- `node npm/test.js`;
- `npm pack --dry-run`;
- shell syntax checks for `install.sh` and `uninstall.sh`;
- Python compilation and fail-closed flag checks for `scripts/test-standalone.py`;
- host native packaging and extracted `spynel --version` execution;
- full host `v0.99.0` to `v1.0.1` standalone verification with `--modern-baseline`;
- retired first-party repository reference search;
- `git diff --check`.

The hybrid quality judgment returned `PASS`. The focused security review returned `PASS` for the final updater, installer, npm, release-workflow, and standalone-verifier delta.
