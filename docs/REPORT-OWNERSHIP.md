# Report ownership during shutdown

This change addresses two source findings in the Node reporting path.

- `SL-REPORT-001`: a periodic report owns its detached tracker batch until the
  sink returns. Shutdown closes periodic admission, waits for an admitted report
  to finish delivery or restoration, then extracts and sends one final batch.
  The final result is retained, so every final-report caller receives the same
  error and cleanup can continue through `Run`'s existing deferred operations.
- `SL-REPORT-002`: alive-IP flushes now allocate detached maps and slices. A
  failed non-nil snapshot requests a retry of the latest live state instead of
  merging stale IPs back into tracker state. Changed empty snapshots remain
  distinguishable from unchanged snapshots.

`reportMu` serializes report snapshot extraction, sink delivery, and failure
restoration. Periodic reporting uses `TryLock`, so the service loop skips a tick
when another report owns the stream. After locking, it checks the stopping flag
before extraction; this is the admission boundary. An admitted asynchronous job
keeps `pushActive` set and retains the mutex until delivery, restoration,
backoff, and logging are complete. Final reporting sets the stopping flag before
waiting for that mutex, preventing any later periodic extraction.

For direct legacy node services, final report errors reach the command's
existing first-error exit decision. Machine child service failures retain the
orchestrator's existing log-only handling and do not become its Run error.
Because one configured instance can expand into several node services, the
command's error channel now retains only the first error with a nonblocking
send and then cancels the run. Both command-level worker error sites keep their
logs; a full error channel can no longer prevent worker completion.

Failed traffic and the alive-IP retry request remain only in process memory.
The final wait depends on `Sink.Report` returning; its existing HTTP timeout is
unchanged. An ambiguous network failure cannot provide exactly-once server
acceptance, and this change does not alter panel payload encoding.

These source changes do not add shutdown kernel-byte collection, core drain
accounting, restart generation handoff, old-core traffic retention, or shared
SingLink adapter work. `SL-ACCOUNT-001` and the broader `SL-ACCOUNT-002` remain
open. The existing deferred kernel stop order is unchanged. This document records
source-level ownership findings and does not claim reproduced faults or runtime
verification.
