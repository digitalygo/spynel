---
status: completed
created_at: 2026-09-19
updated_at: 2026-09-19
files_edited:
  - .github/AGENTS.md
  - .github/workflows/AGENTS.md
  - .github/workflows/release.yml
  - .gitignore
  - AGENTS.md
  - README.md
  - cmd/spynel/main.go
  - docs/AGENTS.md
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
rationale: Migrate Spynel's canonical module, release, installer, updater, and documentation coordinates to the Digitalygo repository without changing supported distribution formats. The npm identity later moved to the scoped public package @digitalygo/spynel while the spynel command and native artifact names stayed unchanged.
supporting_docs:
  - ../research/2026-09-19-spynel-architecture-security-quality-gates.md
  - ../status/2026-09-19-npm-publication-workspace-state.md
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

## Update 2026-09-19: scoped npm package identity

### Summary of changes

The earlier decision to keep the npm package name unscoped as `spynel` was superseded. The npm package identity is now the scoped public package `@digitalygo/spynel`, documented across the README quick start, the getting started guide, the releasing guide, and the npm, documentation, GitHub automation, and root DOX contracts. The first publication still bootstraps with the GitHub Actions repository secret `NPM_TOKEN`; npm Trusted Publishing is then configured for `@digitalygo/spynel`, and the secret is removed after the first successful OIDC publication. The `spynel` CLI command, native archive and binary names, Digitalygo GitHub release coordinates, and supported release formats are unchanged. No credential value is recorded in this or any other repository document.

### Technical reasoning

The repository migration deliberately kept the unscoped npm name to limit its change surface, but that name does not express the Digitalygo ownership now encoded in the repository, Go module, release, and documentation coordinates. The scoped `@digitalygo/spynel` identity namespaces the package under the npm organization, aligns package discovery with the canonical Digitalygo coordinates, and avoids ambiguity with unrelated registry names. Bootstrap ordering is unchanged: npm cannot configure a trusted publisher before a package exists, so the first release uses the `NPM_TOKEN` repository secret, and steady-state publication uses OIDC plus provenance with no retained long-lived credential.

### Impact assessment

- Installation documentation uses `npm install -g @digitalygo/spynel`, while the installed command remains `spynel`.
- npm publisher setup and the `npm trust github` example target `@digitalygo/spynel`; `NPM_TOKEN` is removed after the first successful trusted publication.
- Native archive names, binary names, GitHub release coordinates, update formats, and the `v1.0.0` release bootstrap behavior are unchanged.
- The scoped identity must also hold in the npm manifest and the launcher and updater code before publication; this update covers documentation and DOX contracts only.
- The previous unscoped decision remains visible in the original sections above; this update supersedes it.

### Validation steps

- Re-read every changed document and contract and confirmed that remaining `spynel` references denote the command, module, workspace, binary, or archive rather than the npm package.
- Confirmed that no credential value appears in any changed file.
- Ran the repository DOX coverage gate on the changed contracts.
- Ran `git diff --check` and verified whitespace, heading, and list consistency in the changed files.

## Update 2026-09-19: v1.0.0 publication

### Summary of changes

The scoped package migration was committed as `d13e919` and published in GitHub Release `v1.0.0`. The release contains four native archives and `checksums.txt`; npm accepted and published `@digitalygo/spynel@1.0.0` with signed provenance and the `latest` distribution tag.

### Technical reasoning

The first release attempt targeted the earlier unscoped-package commit and failed during a flaky verification test before building assets. That empty release and tag were removed. After the scoped migration passed local quality and security gates, a manual dispatch on `d13e919` passed all verification and native jobs. The release was then recreated at the same version and completed every publish job.

A granular npm token was used only for the bootstrap publication. npm accepted package publication but rejected creation of the Trusted Publisher because bypass-2FA tokens may no longer perform package trust configuration. The repository secret was removed after publication. Trusted Publishing must therefore be configured through an npm session authenticated with interactive 2FA before the next release.

### Impact assessment

- GitHub Release `v1.0.0` points to `d13e919d0a12fe9fd70e74294d49b828e3efd4ef`.
- Native assets exist for Linux amd64 and arm64 plus macOS amd64 and arm64.
- npm `latest` resolves to `@digitalygo/spynel@1.0.0` and installs the `spynel` command.
- Future release jobs currently have no `NPM_TOKEN` fallback. They require the npm Trusted Publisher for `digitalygo/spynel` and `.github/workflows/release.yml` to be configured first.
- The bootstrap token value is not recorded anywhere in the repository or operation trace and should be revoked or rotated through npm account settings because it was shared through an interactive channel.

### Validation steps

- Manual workflow dispatch `35449337641` passed verify and all four native build jobs for commit `d13e919`.
- Release workflow `35449688043` passed verify, all four native jobs, GitHub asset publication, npm publication, and provenance generation.
- GitHub Release inspection confirmed `checksums.txt` plus all four supported archives.
- npm registry polling confirmed package name, version `1.0.0`, `latest` tag, repository coordinate, and `spynel` bin mapping.
- A clean prefix installation of `@digitalygo/spynel@1.0.0` downloaded the native runtime and returned `spynel 1.0.0`.
- The GitHub Actions `NPM_TOKEN` secret was deleted after the successful bootstrap publication.
