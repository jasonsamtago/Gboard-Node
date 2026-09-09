# Report ownership implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development for the scoped implementation and independent review. Design was delegated by the user; no additional approval is pending.

**Goal:** Serialize periodic/final reports, retain failed traffic batches, and give each alive-status batch independent ownership.

**Architecture:** One report mutex spans extraction/delivery/restoration; periodic TryLock preserves event-loop responsiveness and shutdown seals admission before waiting. Tracker returns detached alive snapshots and invalidates deduplication for a fresh retry after failure.

**Tech Stack:** Existing Go1.26 Node repository; standard library only.

## Global constraints

- Work only in `/Volumes/sing990pro/gboard-new/singlink-v1/Gboard-Node` on existing isolated `codex/singlink-v1`; do not touch the separately owned main worktree.
- User requests no tests: no test files/commands, probes, listeners, clients, traffic, captures, deployments or dependency installs/changes.
- Do not alter any kernel, protocol, Sink interface, panel payload schema, shutdown kernel order or Tracker.Process cumulative semantics. Do not add report workers, timers or goroutines beyond the existing asynchronous report job.
- Compilation/static review are not runtime accounting, race or delivery evidence. SL-ACCOUNT-001/002 and full SingLink integration remain open.

### Task 1: Implement reporting ownership and detached alive snapshots

**Files:** modify `internal/service/service.go`, `internal/tracker/tracker.go`, `cmd/gboard-node/main.go`; create `internal/service/reporting.go`, `docs/REPORT-OWNERSHIP.md`.

Read the full exact contract in `docs/superpowers/specs/2026-09-10-report-ownership.md` first. All API names, states, ordering, errors and boundaries are binding.

- [ ] Add the four service fields from the spec, preserving pushActive as an async observer and pullActive separately. Move the two report functions to reporting.go. Change Run cancellation to `return s.pushReportSync()` while retaining all defers.
- [ ] Implement periodic TryLock with the under-lock stopping recheck, unchanged backoff checks before extraction, and pushActive ordering through report completion. Create one payload then transfer mutex ownership to the existing report goroutine; it calls shared sendReport and releases its ownership after restoration/backoff/logging.
- [ ] Implement finalReportOnce: seal admission, wait on reportMu, extract once, send once, retain/report any error. Subsequent final calls return the same error; no further periodic batch can be taken after admission closes.
- [ ] In cmd/gboard-node/main.go, replace the instance-count-sized error channel with capacity 1 and a shared local recordError helper using nonblocking select then cancel. Keep the existing logs at both worker failure sites. This prevents final-error fan-in from blocking worker completion; no new receiver or lifecycle change. See the exact caller contract in the updated spec.
- [ ] Factor existing payload extraction into reportSnapshot, retaining all CPU/memory/swap/disk/online/metrics/kernel_status fields. Factor common sink call and failure restoration into sendReport; do not duplicate final/periodic restoration logic.
- [ ] Replace reusable tracker alive buffer with fresh maps/slices produced under tracker.mu, and aliveIPsRetry. Preserve unchanged=nil versus changed-empty=nonnil map. RestoreAliveIPs marks retry for nonnil input; next flush uses latest live state, including newly disconnected/connected users. Do not merge stale IP state or mutate another caller's map.
- [ ] Write docs/REPORT-OWNERSHIP.md: source findings SL-REPORT-001/002, sequencing and batch ownership, stable final error and cleanup, in-memory retention, sink-return dependency and remaining kernel collection/restart work. No reproduced-fault, runtime proof or complete SL-ACCOUNT-002 claim.
- [ ] Run gofmt, git diff --check, offline `go build ./internal/service ./internal/tracker ./cmd/gboard-node`, `go vet ./internal/service ./internal/tracker ./cmd/gboard-node`, and `go build ./...` with Go1.26.3 and the specified offline environment. Do not run tests or install anything. Report failures honestly.
- [ ] Commit only the five scoped files. Write the requested report with base/head SHAs, exact commands/status/output, self-review of report admission/close orderings, error fan-in and snapshot aliasing, and limitations. No push; controller handles independent review and draft PR.

The implementation report must distinguish historical evidence from this no-tests source change. The controller will review the task, update the shared issue/source ledger, request a final review, and publish the reviewed draft without merging or deploying.
