# Report ownership during shutdown

Design delegated by the user. This is ordinary Node reporting/lifecycle maintenance. It does not add a protocol, change network transports, execute traffic or retry any blocked integration operation. Latest user instruction prohibits tests; use compilation/static review only.

## Source findings and scope

Baseline `cc37974ab8d323931d828152050c2d9f9965893f`:

- `Service.pushReportAsync` owns a detached traffic batch until Report returns. On failure it restores traffic. `pushReportSync` does not wait for that operation, can flush too early, and discards its own failed batch without restoring it. `Run` returns success even when the final report failed. Track as `SL-REPORT-001`, a reporting component of broader `SL-ACCOUNT-002`.
- `Tracker.FlushAliveIPs` returns its reusable internal map. `sendDeviceBatch` may flush again while a report goroutine still reads it. The returned map/slices must have independent ownership. `RestoreAliveIPs` merges into that buffer but does not invalidate the deduplication hash, so an unchanged current snapshot can still be suppressed after failure. Track as `SL-REPORT-002`.

Choose one service report mutex covering extraction, sink delivery and restoration. Periodic reporting uses TryLock so a slow report cannot block the service event loop; final reporting closes admission and waits for the prior report before extracting the final tracker batch. Use immutable detached alive snapshots and retry the latest observed alive state after failure.

Alternatives rejected: polling pushActive risks a check/flush race; a new queue/worker would add lifecycle complexity and unbounded queue policy; copying the returned alive map outside Tracker's lock still races its next mutation. A mutex held across sink delivery is deliberate serialization of this one report stream; it is not a kernel, tracker or configuration state lock.

## Exact requirements

### Service state and lifecycle

Add fields near existing push state:

```go
reportMu sync.Mutex
reportStopping atomic.Bool
finalReportOnce sync.Once
finalReportErr error
```

Keep `pushActive atomic.Bool` for existing observers; it denotes the asynchronous report job from admitted snapshot creation through sink return/restoration/backoff/logging. Keep pullActive independent. The zero values initialize all new fields; constructors need no special initialization. A Service is one Run lifecycle; no reopening reporting after final shutdown.

Move pushReportAsync and pushReportSync to `internal/service/reporting.go`, together with shared `reportSnapshot() controlplane.ReportPayload` and `sendReport(payload controlplane.ReportPayload) error` helpers. Existing telemetry payload fields and asynchronous backoff behavior must be preserved. Keep unrelated service code in place.

`pushReportAsync` must:

1. Return if reporting is stopping or the sink does not support reporting.
2. TryLock reportMu; if busy, log/skip without waiting or flushing any tracker data.
3. Recheck reportStopping after acquiring the mutex; if stopping, unlock and return. This check is the admission linearization point. A job admitted before shutdown may finish; final shutdown must wait for it.
4. Honor pushBackoff.shouldSkip before taking a batch, unlocking on skip. Set pushActive=true for an admitted job.
5. Create the payload synchronously while holding reportMu (as the old method did before spawning its goroutine), then launch its existing single background reporting goroutine. The goroutine retains the mutex until sendReport, restoration, backoff and logging finish. It clears pushActive before unlocking on return. No extra worker/timer/goroutine is added.
6. Call sendReport. On error, update existing failure backoff and log; on success update success backoff and existing ReportPushed log.

`pushReportSync() error` is shutdown-only, idempotent via finalReportOnce, and retains finalReportErr for concurrent/later callers. Its once body first stores reportStopping=true, then locks reportMu to wait for every already-admitted periodic job to complete delivery/restoration. If reporting is unsupported, return nil without extraction. Otherwise snapshot once and send once, with the same restoration helper on error. Log the final failure and store a wrapping error (or the original error) with errors.Is-compatible wrapping. Keep reportStopping=true after success or failure. Do not silently retry final delivery on a repeated call. In Run's cancellation branch, return this error; existing deferred kernel/certificate/ticker cleanup must still execute.

`reportSnapshot` extracts FlushTraffic, FlushAliveIPs, CurrentOnline, monitor data and buildMetrics with existing kernel_status exactly as before; it is called only while reportMu is held. Traffic is already detached by FlushTraffic. Do not hold tracker.mu or metricsMu across Sink.Report.

`sendReport` calls Sink.Report exactly once. On error it restores nonempty Traffic with RestoreTraffic and requests a fresh alive snapshot through RestoreAliveIPs(payload.Alive), then returns the sink error; on success it does neither. There is no reporting capability/interface or panel payload schema change.

### Command error fan-in required by final-error propagation

Source review of `cmd/gboard-node/main.go:152-218` found errCh capacity is len(instances), but each legacy instance can ExpandNodes into more reporting services. Every service sends its error before completing its wait group, while the receiver waits for doneCh before reading errCh. More failing nodes than buffer slots can therefore block shutdown. Returning final report errors makes this path directly relevant to this change.

Use `errCh := make(chan error, 1)` and one local `recordError := func(err error)` helper that nonblockingly attempts to send via select/case/default, then calls cancel regardless of whether the error fit. Both existing error sites (machine orchestrator and legacy node service) keep their per-error logging and call recordError in place of the blocking send plus cancel. The command already uses only the first error for its exit decision. Preserve channel closure/read after all workers complete, existing cleanup, reload behavior and startup staggering. Do not add a receiver goroutine or drop the existing logs. The helper retains the first failure while allowing every worker to finish even if many services fail together.

### Tracker alive snapshot ownership

Remove the reusable aliveIPsBuf field and its constructor initialization. Keep lastAliveIPsHash; add `aliveIPsRetry bool` protected by Tracker.mu.

`FlushAliveIPs` locks Tracker.mu, loads the live snapshot while holding that lock, computes the existing hash, and returns nil only if the hash is unchanged AND aliveIPsRetry is false. Otherwise it creates a new map and new slice for every user from the live snapshot, updates the hash, clears retry and returns the detached map. A changed empty snapshot is a nonnil empty map; an unchanged snapshot remains nil. Callers can retain/mutate a returned map without changing tracker state or a different returned batch. Process keeps its existing cumulative-traffic semantics.

`RestoreAliveIPs(data map[int][]string)` keeps its signature but changes implementation: nil input is a no-op; any nonnil snapshot, including empty, marks aliveIPsRetry=true under the mutex. It does not retain or merge the caller's map, mutate live state, or resurrect disconnected IPs. The next FlushAliveIPs returns the latest live snapshot even if its hash matches the last returned snapshot. Update its comment to describe fresh-snapshot retry rather than historical merge. This is snapshot status, unlike billable traffic which must be restored additively.

### Scope and limitations

This fix serializes service-owned reports and preserves failed batches only in process memory. It does not make Sink.Report cancellable or persist data; the final wait depends on the sink returning. Existing HTTP client timeout remains unchanged. It does not guarantee exactly-once server acceptance after ambiguous network errors or change empty-field encoding in the panel client.

It does not fix final kernel-byte collection, core drain accounting, restart generation handoff, old-core traffic retention, the shared SingLink adapter, or complete `SL-ACCOUNT-001/002`. In particular Run still collects no new kernel snapshot during shutdown; its deferred Stop order is unchanged. Keep these broader issues open and describe the narrower delivered reporting fix accurately.

## Verification and delivery

No tests, test files, test commands, probes, listeners, clients, traffic, packet captures, installations, dependency changes or deployment. Go 1.26 is this repository's declared minimum; the local Go1.26.3 is used, not Go1.20 from the separate shared library. Baseline offline `go build ./internal/service ./internal/tracker` passed before changes.

Format changed Go files, run offline build/vet for `./internal/service ./internal/tracker ./cmd/gboard-node`, then offline `go build ./...` if cached dependencies allow. Environment: GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local GONOPROXY=none GOVCS=*:off. Inspect exact outputs/exit codes; missing cached dependencies are not passing evidence. Independent static task and whole-change review follow. Submit only an experimental draft PR after review, without merge/deployment or claims of reproduced/runtime-verified faults.
