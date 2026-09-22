---
status: completed
created_at: 2026-09-22
files_edited:
  - internal/app/AGENTS.md
  - internal/app/job_archive.go
  - internal/app/job_archive_test.go
  - internal/app/runtime.go
  - internal/channel/tui/AGENTS.md
  - internal/channel/tui/tui_test.go
  - internal/localapi/AGENTS.md
  - internal/localapi/localapi_test.go
  - internal/localapi/server.go
  - substrate/traces/operations/2026-09-22-job-archive-shutdown-quiesce.md
rationale: Record the completed shutdown quiesce fix that fences terminal job-archive writes at close and joins detached local-API dispatch before Serve returns, plus the DOX pass describing the new contracts.
supporting_docs:
  - 2026-09-21-pi-session-topic-naming.md
  - ../../../internal/app/AGENTS.md
  - ../../../internal/localapi/AGENTS.md
  - ../../../internal/channel/tui/AGENTS.md
---

# Job archive shutdown quiesce

## Summary of changes

A shutdown fix makes runtime teardown deterministic for job-archive persistence. `JobArchive.close` now sets a permanent closed boundary under the archive mutex, every archive operation refuses after that boundary, and `Runtime.Close` retires the archive before it returns, so a terminal snapshot write can no longer land in the workspace after close. The local API `Serve` loop now tracks the detached `/v1/message` application dispatches and joins them, bounded by the five-second shutdown timeout, before returning. `startTestServer` closes the service before `t.TempDir` cleanup, which removes the cleanup race that intermittently failed `TestSocketLifecycle` during CI and release validation.

The production changes live in `internal/app/job_archive.go`, `internal/app/runtime.go`, and `internal/localapi/server.go`. Tests were extended or adjusted in `internal/app/job_archive_test.go`, `internal/localapi/localapi_test.go`, and `internal/channel/tui/tui_test.go`. This record also captures the DOX pass that makes the new contracts binding in `internal/app/AGENTS.md`, `internal/localapi/AGENTS.md`, and `internal/channel/tui/AGENTS.md`. Nothing is committed; every file is unstaged in the working tree.

## Technical reasoning

### Root cause chain: wrapEmit -> EndJob -> AtomicWriteFile racing RemoveAll

`Service.wrapEmit` handles a terminal provider event in this order: it persists the terminal history entry, records the event snapshot through `Runtime.RecordJobEvent`, prepends the optional Pi session notice, hands the event to `downstream(event)` (the transport emitter), and only then calls `Runtime.EndJob`. `EndJob` calls `archive.finish`, which renders the final record and writes it through `writeLocked` and `fsx.AtomicWriteFile`.

The transport observes completion at `downstream(event)`. For a local `/v1/message` client, that is the moment the terminal NDJSON frame is encoded and flushed; the HTTP handler can return while the same `Service.Handle` goroutine is still inside `wrapEmit` and has not yet run `EndJob`. Three gaps turned that ordering into a real race during shutdown:

- `Runtime.Close` never called `JobArchive.close`. The close method existed but only two tests called it to simulate process exit, so production shutdown left the archive open.
- `writeLocked` did not check the archive's existing `boundary` field, which `newJobArchive` set only when directory setup failed. Even a close call could not stop a later `update`, `event`, or `finish` write.
- `localapi.Serve` did not track the `/v1/message` dispatcher goroutine, so `Serve` could return while the application write was still pending. Primary shutdown then called `Runtime.Close`, and test cleanup ran `t.TempDir` removal (`RemoveAll`) concurrently with `fsx.AtomicWriteFile`, which recreates path components. That is the `TestSocketLifecycle` TempDir flake already noted in the 2026-09-21 Pi session naming record, and it blocked the pending v1.5.0 release validation.

### Close fencing

`JobArchive.close` sets `a.boundary = errJobArchiveClosed` while holding `a.mu`. Every write path holds that mutex across its full read-modify-write, so acquiring it waits out any in-flight write; after close returns, `writeLocked` returns the boundary error before `ensureJobArchiveDirectory` or `AtomicWriteFile` runs. A late write therefore cannot recreate the jobs directory or replace the file. `allocate`, `begin`, `recent`, `get`, and `cleanup` already refused on a non-nil boundary; the new check in `writeLocked` extends the same fence to `update`, `event`, and `finish`, which are the paths a late terminal event takes.

Refusals surface through `Runtime.reportJobArchiveError` as `job_archive/<operation>_failed` error entries in the runtime log. The on-disk record keeps its last persisted snapshot because the refused write never touches the filesystem. `finish` still deletes the in-memory entry and releases its ownership locks after the refused write, so a late terminal transition is dropped rather than persisted into the archive.

### Shutdown ordering

`Runtime.Close` now reads `r.archive` under the runtime mutex, releases that mutex, and only then calls `archive.close()`. No runtime lock is held while waiting on the archive mutex, and the pre-existing runtime-then-archive order used by `tryBeginJobWithDetails` (which calls `archive.allocate` while holding `r.mu`) keeps its direction. Nothing acquires the runtime mutex from inside the archive, so the ordering introduces no inversion.

`primaryTerm.stopFor` installs `defer term.service.Runtime.Close()` inside its `stopOnce` closure. The closure cancels the context, closes the listener and harness, and only returns after `<-term.apiDone`, `<-term.channelsDone`, `<-term.orchestratorDone`, and the orchestrator wait. The deferred close therefore runs after both `Serve` calls return, which after this change includes the bounded dispatch drain. API drain runs before the archive fence.

### Local API dispatch drain

`Serve` creates one `dispatchScope` (a `sync.WaitGroup`) and passes it to the `/v1/message` handler. The handler derives its context from `request.Context()` and runs `Service.Handle(ctx, message, emit)` in a goroutine that calls `scope.dispatch.Done()` when the call returns. The server's `BaseContext` carries the `Serve` context, and shutdown calls `cancelServer()` before `shutdownServer`, so request contexts are canceled and the detached dispatch sees that cancellation. After `http.Server.Serve` returns and `shutdownServer` completes, `Serve` waits on the scope with a separate waiter goroutine against a `time.After(shutdownTimeout)` channel. `shutdownTimeout` is five seconds. Normal shutdown waits for in-flight dispatches; a stalled dispatch cannot hold shutdown open past the bound, and the released but still-running goroutine is the accumulation edge recorded as a security advisory.

### Test hygiene

`startTestServer` now registers `t.Cleanup(func() { _ = service.Close() })` right after constructing the service. `t.TempDir` registered its removal cleanup first, and Go runs cleanups last-in-first-out, so the service close (and with it the archive fence) runs before the directory is removed. `Service.Close` is idempotent through `Runtime.closeOnce`, so tests that already close explicitly are unaffected. This is the change that removes the CI TempDir flake.

The separate TUI repeat-run flake came from two raw `View()` byte tests whose SGR bytes could be reordered by a color profile installed by another test in the same process. Both tests now capture `lipgloss.ColorProfile()`, set `termenv.Ascii`, and restore the captured profile through `t.Cleanup`. Assertions are unchanged.

## Impact assessment

- Shutdown is deterministic. `Runtime.Close` returning means no job-archive write is running or can start, and `Serve` returning within its five-second bound means the listener's detached application dispatches settled. Workspace teardown can no longer race a late terminal snapshot.
- A terminal snapshot update after the fence is refused, not merged. The archive keeps its last persisted state and the refusal is logged. This is intended behavior after shutdown, and the security review recorded it as a `finish()` post-fence semantics advisory.
- Normal operation is unchanged: no configuration key, state schema, API payload, workflow rule, or user-visible behavior change. Records written before shutdown keep their previous content.
- The local API TempDir cleanup race that intermittently failed `TestSocketLifecycle` is gone, and the two TUI raw-view tests are deterministic across repeated runs.
- DOX contracts now state the fencing and quiesce rules in `internal/app/AGENTS.md`, `internal/localapi/AGENTS.md`, and the profile-pinning test rule in `internal/channel/tui/AGENTS.md`. No child index or parent contract changed.

## Validation steps

### Fix-work evidence

Recorded by the implementation and gate work on this tree:

- `scripts/dev.sh test` passed, covering the full Go suite plus the nested Bubble Tea module tests.
- Race runs passed: `go test -race -count=3 ./internal/localapi` and the `internal/app` race suite. The app race run used an explicit timeout, and the orchestrator's default-timeout invocation of that suite was green as well.
- `go vet ./...` reported no findings.
- `mkdir -p .tmp-bin && go build -o .tmp-bin/spynel ./cmd/spynel` built the binary successfully.
- `scripts/smoke.sh` passed.
- `scripts/dev.sh dox` passed.
- Function coverage measured on the changed delta: `JobArchive.close` 90.9 percent, `Runtime.Close` 73.3 percent, `Serve` 90.9 percent, `shutdownServer` 75 percent, `writeLocked` 62.5 percent.
- Quality gate: PASS with two advisories, one of them the missing direct `Serve`-quiesce assertion. The drain is exercised through the suite rather than by a dedicated test that asserts `Serve` blocks on an in-flight dispatch.
- Security gate: PASS with two advisories, the stuck-dispatch goroutine accumulation note and the `finish()` post-fence snapshot semantics note.
- Test-to-behavior mapping: `TestRuntimeCloseFencesJobArchiveWrites` asserts that late `RecordJobEvent`, `UpdateJob`, `EndJob`, and `TryBeginJobWithDetails` calls after `Runtime.Close` leave the job-archive directory byte-for-byte unchanged; the localapi suite exercises the cleanup ordering through `startTestServer`; the two TUI tests pin the ASCII profile around their view-byte assertions.

### Documentation pass

This record and the DOX edits were checked on the final working tree:

- `scripts/dev.sh dox` reported `DOX coverage valid for 51 tracked directories`.
- `go test -count=1 ./internal/app ./internal/localapi` passed: `internal/app` 14.640s, `internal/localapi` 0.428s.
- `git diff --check` reported no whitespace errors on tracked changes, and the new record was checked separately for trailing whitespace and a single trailing newline.
- Markdown structure was verified for the three edited AGENTS.md files and this record: one H1 each, uninterrupted heading progression, no em dash, no trailing whitespace, and a single trailing newline at end of file.

## Release status

This fix is part of the pending v1.5.0 release and unblocks its release validation. The intermittent CI TempDir flakes occurred while validating that release, and the fence plus the drain remove the failure mode. The release itself is not prepared, tagged, or published by this work, and no commit exists for it: all six modified files, the three AGENTS.md contracts, and this record remain unstaged in the working tree.

## References

- [Prior record of the TestSocketLifecycle TempDir flake, Pi session and topic naming](2026-09-21-pi-session-topic-naming.md)
- [Application service DOX](../../../internal/app/AGENTS.md)
- [Local API DOX](../../../internal/localapi/AGENTS.md)
- [Terminal UI DOX](../../../internal/channel/tui/AGENTS.md)
