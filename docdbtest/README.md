# DocumentDB verification test suite

Runs the verifier against a **DocumentDB source** and a **MongoDB/Atlas
destination**, captures a log per scenario, reviews those logs for known
failure signatures, and tears down everything it created.

Intended to run on an **EC2 host inside the DocumentDB VPC**. It works through
an SSH tunnel, but that makes the throughput numbers meaningless: the change
reader's `getMore` loop is round-trip bound, and a tunnel penalises it far more
than it penalises batched writes.

## Running it

Two environment variables, one command:

```bash
export SRC_URI='mongodb://USER:PASS@docdb-endpoint.region.docdb.amazonaws.com:27017/'
export DST_URI='mongodb+srv://USER:PASS@atlas-cluster.mongodb.net/'

./docdbtest/run_suite.sh
```

That is the whole procedure. The suite handles the rest itself:

- **Downloads the DocumentDB CA bundle** if it is not already present.
- **Fixes up the source URI** — appends `tls`, `tlsCAFile`, `directConnection`
  and `retryWrites=false` if you did not supply them, without duplicating
  anything you did. It keeps a separate `mongosh` variant, because `mongosh`
  understands `tlsAllowInvalidHostnames` and the Go driver does not.
- **Finds or builds the verifier** — `$MV_BIN`, else a binary in the repo root,
  else `go build`.
- **Checks connectivity** to both clusters before doing any work, so a
  misconfigured security group or Atlas IP access list fails in seconds rather
  than halfway through.
- **Enables the change streams it needs** and records which ones it turned on.
- **Seeds and verifies** every scenario.
- **Reviews the logs** and prints the analysis.
- **Tears everything down** on exit, including on failure or Ctrl-C: change
  streams it enabled are disabled again, and the test databases plus the
  verifier's metadata database are dropped.

## Options

| Variable | Default | Purpose |
|---|---|---|
| `META_URI` | `DST_URI` | Metadata cluster. Must not be DocumentDB. |
| `MV_BIN` | repo binary, else built | Path to the verifier. |
| `OUT_DIR` | `./docdbtest-logs-<ts>` | Where logs go. |
| `DOCS` | 50000 | Documents per collection in the baseline. |
| `SCALE_COLLS` / `SCALE_DOCS` | 5 / 50000 | Scale test shape. |
| `TESTS` | all | Subset, e.g. `TESTS="gates baseline throughput"`. |
| `KEEP_DATA=1` | off | Skip teardown, to investigate a failure. |
| `AUTO_INSTALL=1` | off | Let the suite `sudo` install `mongosh` if missing. |

`mongosh` is the one thing the suite will not install without permission, since
that needs root. Without `AUTO_INSTALL=1` it exits with the exact command for
your platform.

## What each test does

| Test | Checks |
|---|---|
| `gates` | The four DocumentDB rejections fire: `tailOplog`, `toHashedIndexKey`, secondary read preference, DocumentDB metadata cluster. |
| `baseline` | Identical data verifies clean. Every supported BSON type, all 10 index types, and a collection whose `_id`s span 12 BSON types. |
| `mismatch` | Six injected differences — changed field, deleted doc, extra doc, dropped index, altered index option, changed field in the mixed-`_id` collection — are all detected. |
| `crud` | `insert`, `update`, `replace`, `delete` all flow through the change reader into rechecks. |
| `writesoff` | The writes-off fence and quiescence drain complete rather than hang. |
| `resume` | Restarting without `--clean` resumes from the persisted token. |
| `throughput` | Six parallel writers flood the source for 60s; measures how far the reader lags and whether it consumes every event. **The number most worth having.** |
| `verifyall` | `--verifyAll` passes the cluster-wide change stream gate and verifies all namespaces. Skips itself if cluster-wide streams could not be enabled. |
| `scale` | Multiple collections at volume verify clean. |

## Reading the output

`RESULTS.txt` gives pass/fail per test; `ANALYSIS.txt` is the log review, which
the suite runs automatically. Findings are graded:

- **CRITICAL** — a wrong or untrustworthy result, or a crash. Do not trust the run.
- **WARN** — works but degraded, or worth understanding.
- **INFO** — context.

The analyzer looks for the specific failures encountered while building
DocumentDB support. Each cost a debugging session to find the first time:

- `Feature not supported: 'causal consistency'` (code 303) — appears only on a
  change-stream **resume**, so typically during a failover, long after the run
  looked healthy.
- `Feature not supported: $$REMOVE` — the DocumentDB pipeline branch was not
  taken; check flavor detection.
- `` unexpected `v` value `` — DocumentDB reports index `v: 4`, MongoDB `v: 2`.
- `CappedPositionLost` — the resume token aged out. The run must restart with
  `--clean`; continuing would resume across an unobserved gap.
- **A clean report over zero documents compared** — the worst outcome, and it
  happened for real: an unequal `capped` value made collections look
  uncomparable, so generation 0 skipped every document and still reported only
  a metadata mismatch.

You can also run the review by hand against any log directory:

```bash
./docdbtest/analyze_logs.sh docdbtest-logs-<timestamp>
```

## Things worth knowing before interpreting results

- **Retention.** DocumentDB keeps change stream events for 3 hours by default.
  Raise `change_stream_log_retention_duration` before any long run, or a reader
  that falls behind will abort with an expired token. The suite cannot set this
  for you: it lives in a cluster parameter group, and `getParameter` is not
  implemented on DocumentDB, so it cannot even be read over the connection.
- **Bulk writes.** DocumentDB's multi-document writes are atomic, so an
  `updateMany` still running when writes-off is called commits entirely after
  the fence and is excluded wholesale. Let bulk operations finish first.
- **Index DDL is invisible.** DocumentDB emits no change events for index
  creation or drops, so index changes mid-run are never noticed. Freeze index
  changes for the duration of a verification.
- **Only DocumentDB 5.0.0 has been validated.** 3.6, 4.0 and 8.0 differ
  materially — 4.0 lacks `partialFilterExpression`, 3.6/4.0 allow change
  streams only from the primary, 8.0 uses a different query planner. Run
  `architecture/documentdb_probe.js` against your actual version first.

## Related

- `architecture/documentdb_open_questions.md` — every assumption the support
  rests on, and how each was confirmed.
- `architecture/documentdb_probe.js` — re-checks most of them in one run.
