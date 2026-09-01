# DocumentDB support: assumptions to confirm

This file tracks every assumption the DocumentDB source support rests on, so
that the ones we haven't proven don't get silently forgotten. Add to it
whenever DocumentDB work depends on behavior we haven't observed directly.

## Status legend

| Status | Meaning |
| --- | --- |
| **DOC** | Confirmed from AWS documentation, but not observed on a live cluster. |
| **OPEN** | Not confirmed. Code depends on it. |
| **CLUSTER** | Confirmed by running against a real DocumentDB cluster (note version + date). |

An item is only done when it reaches **CLUSTER**, because DocumentDB emulates
MongoDB's wire protocol: a thing can look supported and behave differently.

## Results: DocumentDB 5.0.0, 2026-09-01

Run via `documentdb_probe.js` against `maruti-poc-docdb-cluster`
(engine 5.0.0, `versionArray [5,0,0,0]`, `setName rs0`, ap-south-1).

Confirmed working as designed: **Q1, Q5, Q6, Q7, Q9, Q11, Q13, Q17, Q20, Q21**.

Most importantly, **Q1 held**: 300/300 writes were immediately visible to a
following primary read. The live-verification design rests on that, so it is
no longer an assumption.

**Q12 confirmed the negative**: `listCollections` reports no `info.uuid`, so
the step-6 fix from `Fatal()` to a returned error was necessary.

**Q13 resolved better than expected**: DocumentDB rejects capped collections
outright (`Feature not supported: capped:true`), so the whole capped-vs-
`$natural` problem cannot arise.

**Q10 confirmed covered**: DocumentDB plans the partitioner's boundary query
as `LIMIT_SKIP -> IXONLYSCAN`, an index-only scan. The first probe run reported
this as a FAIL only because DocumentDB's explain has no `totalDocsExamined`
counter; the probe now reads the stage name instead.

Real code exercised successfully against this cluster:
`TestFindNextIDBoundary` (the index-walk partitioner), `TestGetCollectionUUID`
(the step-6 fix, which returned a clean error rather than `os.Exit(1)`), and
`TestFlavorAgainstLiveServer` (detection, `GetClusterInfo`, the
`operationTime` timestamp path, and the `--srcFlavor` override).

**Second run, change streams enabled** (scoped to the probe database, then
disabled again): **14 pass, 0 fail.** Q2, Q3, Q15 all confirmed.

## End-to-end verification, 2026-09-01

50,000 documents covering every BSON type DocumentDB supports, plus a
12-document collection whose `_id`s span 12 different BSON types, plus all 10
supported index types (single, compound, multikey, nested, unique, sparse,
partial, TTL, 2dsphere, descending). DocumentDB accepted every type and index.
Source DocumentDB 5.0.0 -> destination MongoDB 7.0.40 replica set.

**Result: 50,021 documents compared at 8,097/sec, no mismatches.**

Four bugs blocked this, each found only by running it, all now fixed: Q14
(`$$REMOVE`), Q24 (capped skipping all verification), Q23 (`v: 4` aborting the
run), and the `$expr` false alarm described under Q28.

**Mismatch detection verified**: six deliberate differences were injected into
the destination (changed field, deleted document, extra document, dropped
index, altered index option, changed field in the mixed-`_id` collection). All
six were detected and correctly categorised, including source-only vs
destination-only documents.

**MongoDB -> MongoDB regression**: a clean pass on the same build, confirming
the flavor gates did not disturb the original path.

Still open: **Q4** (resume expiry — needs a timed retention test), **Q8**
(failover), **Q14** (full change stream pipeline), **Q16** (DDL event types),
**Q22** (drain termination). **Q23** and **Q24** need code.

## Operational behaviours verified, 2026-09-01

Beyond the numbered questions, these paths were exercised against the live
cluster:

**Safety gates all fire.** `--srcChangeReader tailOplog`,
`--docCompareMethod toHashedIndexKey`, `--readPreference secondary`, and a
DocumentDB `--metaURI` are each rejected at startup with an explanatory error.
`--partitioningScheme natural` is rejected too, but only once a collection is
large enough to need partitioning — small collections short-circuit before the
scheme is consulted, and take a single-partition path that does not use
`$natural`, so they are correct either way.

**Resume-expiry gate works end to end.** Ageing the persisted token's
timestamp and restarting without `--clean` makes the verifier abort rather
than resume across the gap, with the `--clean` remediation in the message.
This is the behaviour that Q4 showed was previously failing open.

**All four CRUD change types flow through the recheck path.** A run against
matching collections, with an update, a replace, a delete, and an insert
applied to the source only, saw exactly one event of each type and reported
all four correctly: the update and replace as field mismatches, the delete as
present-only-on-destination, the insert as present-only-on-source.
DocumentDB does emit `replace`; the earlier DDL probe missed it only because
that probe used `$set`.

**`--verifyAll` correctly refuses** when change streams are enabled per
database rather than cluster-wide, naming what is enabled and how to fix it.

**A source collection drop aborts the run** with `unknown optype`, as designed.
Per Q16 this is the only DDL DocumentDB can emit, so it is the only DDL that
can ever stop a verification.

### Also exercised

**Document comparison modes.** With 500 documents holding identical content in
different BSON field order, `--docCompareMethod binary` reports mismatches and
`--docCompareMethod ignoreOrder` reports clean — each doing exactly what its
name says.

**Namespace remapping.** `mvtest.origname` on the source verified against
`remapdb.newname` on the destination: clean, 300 documents.

**`--verifyAll`.** With change streams enabled cluster-wide it passes Gate A
and verifies every namespace: 4 namespaces, 52,338 documents. Note that a
collection existing on only one side is **fatal in either direction** under
verifyAll, not a reported mismatch.

**Scale.** 5 collections totalling 250,000 documents verified in 28.3 s at
8,824 docs/sec with 10 workers, clean, no errors. Throughput over an SSH
tunnel to ap-south-1; in-VPC would be faster.

**Bulk writes and the drain.** A 50,000-document `updateMany` takes ~11 s on
this cluster. Two cases, and the difference matters:

- *Contract honoured* — the `updateMany` completes, then writes-off is called:
  all 50,000 changes are caught through the recheck path (55,959 mismatch
  records across three generations).
- *Contract violated* — writes-off called while the `updateMany` is still
  running: those changes are **not** caught, and the run reports clean.

The second case is operator error by definition — writes-off means writes have
stopped. But it is worth stating because DocumentDB makes it sharper than
MongoDB does: DocumentDB's multi-document writes are atomic, so a bulk
operation straddling the fence commits entirely after it and is excluded
wholesale. On MongoDB the same bulk write produces per-document oplog entries,
so the portion before the fence would be caught. **Wait for bulk operations to
finish before calling writes-off.**

### High write throughput

Six parallel writers issuing batched `insertMany` drove **1,380,500 inserts in
60 seconds (~23,000/sec)** into a watched collection while a verification ran.

**No events were lost.** The change reader eventually consumed exactly
1,380,500 of 1,380,500 events, drained, and the final generation reported
exactly 1,380,500 documents as destination-missing — matching ground truth
document for document.

**But it consumed them at only ~800-1,400 events/sec** — roughly a 17-20x
shortfall against the write rate. A 60-second burst took ~20 minutes to work
through.

Two consequences for capacity planning:

1. Under sustained writes above roughly 1-2k/sec the recheck queue grows
   without bound and the verification never converges.
2. DocumentDB retains change stream events for 3 hours by default. A reader
   falling behind at this ratio can outrun the retention window and hit the
   Q4 expiry abort, forcing a restart from a full scan. Raising
   `change_stream_log_retention_duration` is the mitigation.

**Treat the rate as a floor, not a verdict.** This was measured over an SSH
tunnel to ap-south-1. The writers hit 23k/sec through the *same* tunnel using
one round trip per 500 documents, whereas the reader's `getMore` loop is far
more round-trip-bound and so is penalised disproportionately by latency.
Re-measure from inside the VPC before sizing anything on it.

### Not yet exercised

- The same throughput measurement from inside the VPC, which is the number
  that would actually matter operationally.
- Collections individually larger than ~50 MiB.

### Unrelated upstream bug found

`--checkOnly` panics with a nil pointer dereference: it calls `WritesOff`
before `initializeChangeReaders`, so `srcChangeReader` is nil. Reproduced
MongoDB-to-MongoDB, so it is not DocumentDB-specific and is not fixed here.
Drive runs through `--start` plus the `/api/v1/writesOff` endpoint instead.

## Prerequisites for checking these

Most checks need a DocumentDB cluster with change streams enabled and a
collection holding enough documents to be interesting (≥ a few thousand for the
partitioning items).

```js
// Enable change streams cluster-wide.
db.adminCommand({modifyChangeStreams: 1, database: "", collection: "", enable: true})
```

Connect with `readPreference=primary` — DocumentDB 3.6/4.0 only allow change
streams from the primary, and the verifier requires primary reads regardless
(see Q9).

---

## Priority 1 — correctness. Wrong answers here mean a false "clusters match".

### Q1. Primary reads are read-after-write consistent — **CLUSTER** (5.0.0, 2026-09-01)

> **Confirmed.** 300/300 documents were visible to an immediately following
> primary read. The consistency substitution in `compare.go` is sound.

**Assumption.** A read issued to the DocumentDB primary observes every write
acknowledged before that read began.

**Why it matters.** This is the single load-bearing assumption of the whole
design. Without gossiped cluster time we cannot pin reads with
`afterClusterTime`, so instead we open the change reader first and rely on
real-time ordering: a write either commits before our scan read (and the
primary shows it to us) or after (and the change stream captures it). If
primary reads are not read-after-write consistent, documents can slip through
the seam and the verifier reports success on mismatched data.

**Source.** AWS: a `primary` read preference "yields read-after-write
consistency"; reads from the primary are "strongly consistent."

**How to check.** Write a document, then immediately read it back on a fresh
connection with `readPreference=primary`, in a tight loop across a few thousand
iterations. Any miss falsifies the design.

### Q2. Change events always carry `clusterTime` — **CLUSTER** (5.0.0, 2026-09-01)

> **Confirmed.** A real insert event carried keys
> `_id, operationType, clusterTime, ns, documentKey, fullDocument`, with
> `clusterTime` a Timestamp. The per-event timestamp the recheck ordering and
> the drain depend on is present.
>
> Note `updateDescription` is absent, matching AWS's documented divergence.
> Our change stream pipeline removes that field anyway.

**Assumption.** Every change event document includes a `clusterTime` BSON
timestamp.

**Why it matters.** With the resume token unparseable (Q3), the event's
`clusterTime` becomes our only per-event timestamp. It drives recheck
ordering (`recheck_persist.go:180`), the writes-off drain (Q7), and lag
reporting. `recheck_persist.go:131` currently only *warns* when it is missing.

**Source.** AWS change-streams examples show `'clusterTime': Timestamp(...)`
on insert and update events.

**How to check.** Watch a collection, generate insert/update/delete/replace
events, and confirm `clusterTime` is present and non-zero on each.

### Q3. Resume token `_data` is not a MongoDB KeyString — **CLUSTER** (5.0.0, 2026-09-01)

> **Confirmed, decisively.** A captured token,
> `{"_data": "016a96c50200000009010000000000004324"}`, is rejected by our own
> decoder with `unknown keystring ctype 106`. That token is now pinned in
> `internal/keystring/docdb_token_test.go`.
>
> So `positionTimestamp` routing DocumentDB away from
> `extractTSFromChangeStreamResumeToken` is required, not defensive: decoding
> would fail outright.

**Assumption.** `extractTSFromChangeStreamResumeToken` (`change_stream.go:500`)
cannot decode a DocumentDB token, so we must not call it.

**Why it matters.** The current code decodes `_data` as KeyString V1 and reads
element 0 as a timestamp. On a DocumentDB token this fails or, worse, yields a
plausible-but-wrong timestamp, which would silently corrupt the writes-off
fence.

**Source.** AWS examples show tokens like
`{'_data': '015daf94f600000002010000000200009025'}`, which is not the KeyString
encoding. (The leading bytes *look* like a unix timestamp, but we deliberately
do not depend on an undocumented format.)

**How to check.** Capture a real token and confirm
`keystring.KeystringToBson(keystring.V1, ...)` rejects it. Also confirm the
token stays opaque-but-valid when passed back via `resumeAfter`.

### Q4. Resume-token expiry is detectable — **CLUSTER** (5.0.0, 2026-09-01) — gate was wrong, now fixed

> **The gate was failing open.** DocumentDB reports an expired token as
> **code 136**, `"CappedPositionLost: CollectionScan died due to position in
> capped collection being deleted."` It names the mechanism — its change stream
> log is a capped collection — rather than the meaning, and shares neither the
> code nor any wording with MongoDB's ChangeStreamHistoryLost (286).
>
> The step-5 gate matched 286 plus four MongoDB phrasings, none of which hit.
> A verifier resuming after its token aged out would have continued **across an
> unobserved gap** and reported a match it could not justify.
>
> `IsChangeStreamHistoryLostError` now matches code 136 and
> "cappedpositionlost", with a regression test carrying the real error.
>
> A malformed token is distinct: **code 9**, `"Invalid resume token"`. That is
> deliberately *not* treated as lost history, since it indicates corruption
> rather than expiry.
>
> Reproduced without waiting out a retention window by resuming from a token
> whose timestamp prefix was rewritten to a past date.

**Assumption.** Resuming with a token older than the retention window returns a
distinguishable error rather than silently starting from somewhere else.

**Why it matters.** Retention defaults to 3 hours. If the verifier restarts
after a longer outage and the resume silently succeeds from a later point,
there is an unobserved gap in the change stream and the verification is void.

**Implemented in step 5, but on a guess.** `util.IsChangeStreamHistoryLostError`
now aborts the run with an instruction to restart using `--clean`. It matches
MongoDB's code 286 plus several conventional message phrasings, because
DocumentDB's actual code and wording are undocumented. **If DocumentDB uses
neither, this gate silently fails open** — the worst outcome in this file. This
is the single highest-value item to confirm.

**How to check.** Set `change_stream_log_retention_duration` to its 1-hour
minimum, capture a token, wait out the window, and attempt `resumeAfter`.
Record the exact error code and message, then add them to
`IsChangeStreamHistoryLostError`.

### Q5. `$listChangeStreams` works via the driver's `Database.Aggregate` — **CLUSTER** (5.0.0, 2026-09-01)

> **Confirmed.** The stage runs and returns an empty result set when nothing is
> enabled, which is exactly what Gate A needs in order to fail closed.

**Assumption.** `admin.aggregate([{$listChangeStreams: 1}])` as issued by
`listEnabledChangeStreams` (`documentdb.go`) returns the enabled scopes.

**Why it matters.** This is Gate A, which prevents the worst failure mode:
verifying namespaces whose changes nobody is watching. If the call errors, the
gate fails closed (an error, not a false pass) — but the verifier is then
unusable, so we need to know.

**Source.** AWS documents the raw form
`{aggregate: 1, pipeline: [{$listChangeStreams: 1}], cursor: {}}` returning
`{database, collection}` documents, with `""` as a wildcard. The Go driver's
`Database.Aggregate` sends `aggregate: 1`, so these should be equivalent.

**How to check.** Enable change streams at collection, database, and cluster
scope in turn, and confirm each shows up with the documented field names.

### Q6. `readConcern: {level: "majority"}` is accepted — **CLUSTER** (5.0.0, 2026-09-01)

> **Confirmed accepted.** No change needed: `buildClientOpts` can keep setting
> majority for DocumentDB connections.

**Assumption.** DocumentDB tolerates the majority read concern that
`buildClientOpts` sets (`migration_verifier.go:304`).

**Why it matters.** If DocumentDB rejects it, every source read fails. There is
an existing escape hatch (`--ignoreReadConcern`), but if majority is
unsupported we should skip it automatically for DocumentDB rather than making
users discover the flag.

**Narrowed by step 4.** Source `find` commands no longer send a `readConcern`
at all when the source is DocumentDB, so this now only concerns the
client-level majority read concern that `buildClientOpts` sets. If DocumentDB
rejects that, we must skip it for DocumentDB connections too.

**How to check.** `db.coll.find().readConcern("majority")` and a raw `find`
with an explicit `readConcern` document.

### Q7. `operationTime` is present in command responses — **CLUSTER** (5.0.0, 2026-09-01)

> **Confirmed.** `ping`, `dbStats`, and `listDatabases` all return it, as does
> `hello` (Q21). Plenty of fallbacks if `hello` ever stops.

**Assumption.** DocumentDB returns a top-level `operationTime` we can read to
mint a server timestamp.

**Why it matters.** It replaces both `appendOplogNote` (which DocumentDB lacks)
for the writes-off fence and `sess.ClusterTime()` for advancing the reader's
clock while the stream is idle. Needed for steps 4 and 5.

**Source.** An AWS error-response example includes
`"operationTime" : Timestamp(1603461817, 493214)`, which is suggestive but is
an error path, not a documented guarantee.

**Now load-bearing.** Step 4 wired this in: `util.GetSessionTimestamp` reads
`sess.OperationTime()` for DocumentDB instead of `sess.ClusterTime()`, and the
change stream takes its start timestamp from it. The driver populates
`OperationTime` from the reply's `operationTime` field, so if DocumentDB omits
it, opening a change stream fails with "session has no operationTime".

**How to check.** Run `hello`, `ping`, and `dbStats` via `runCommand` and
inspect the raw replies for `operationTime`. Confirm it advances after writes.

### Q8. Change stream survives a failover without a gap — **CLUSTER** (5.0.0, 2026-09-01) — no gap

> **No events were lost.** A real primary restart was performed while a writer
> inserted sequential `_id`s into the source every 200 ms and the verifier
> watched.
>
> The failover produced 30 `ECONNRESET` write failures and **crashed the
> verifier** (see Q29). It was restarted ~2 minutes later and resumed from its
> persisted token.
>
> Gap analysis: of the documents the source actually held that were written
> before the writes-off fence, **872 of 872 were reported** as
> destination-missing. **Gap count: 0** — across a failover, a crash, two
> minutes of downtime, and a token resume.
>
> The verifier reported 1650 documents in total, more than the 872 pre-fence
> ones, because it also caught post-fence writes that arrived before the drain
> ended. Extra rechecks are the safe direction.
>
> Note the writer's acknowledged count (1131) exceeds what the source held from
> before the fence, because ~12 writes that returned `ECONNRESET` during the
> failover had in fact been applied — ordinary non-retryable-write ambiguity,
> not a verifier issue.

**Assumption.** After a primary failover, the reader resumes from its persisted
token with no unobserved changes, and primary read-after-write consistency
(Q1) still holds against the new primary.

**Why it matters.** DocumentDB promotes a replica against the same shared
storage, so this *should* hold, but failover is exactly when a consistency
model tends to bend. This is the scenario I would least want to guess about.

**How to check.** Run a verification against a cluster under write load, force
a failover (`aws docdb failover-db-cluster`), and confirm the reader resumes
and no mismatches are missed.

---

## Priority 2 — behavior. Wrong answers mean crashes or bad performance.

### Q9. Flavor detection is correct on a real cluster — **CLUSTER** (5.0.0, 2026-09-01)

> **Confirmed.** `hello` returned `me=...docdb.amazonaws.com:27017`,
> `setName=rs0`, and no `$clusterTime` — precisely the signature
> `flavorFromHello` keys on. Auto-detection identifies DocumentDB correctly;
> `--srcFlavor` is not needed.

**Assumption.** DocumentDB's `hello` advertises replica-set membership (`me`)
but omits `$clusterTime`, which is how `flavorFromHello`
(`internal/util/clusterinfo.go`) distinguishes it from MongoDB.

**Why it matters.** Every DocumentDB code path keys off this. A false negative
sends a DocumentDB cluster down MongoDB paths and it fails confusingly; a false
positive disables MongoDB features on a real MongoDB cluster.

**Mitigation already in place.** `--srcFlavor documentdb|mongodb` overrides
detection.

**How to check.**

```js
const h = db.adminCommand({hello: 1})
print("me:", h.me, "| setName:", h.setName, "| $clusterTime:", h.$clusterTime !== undefined)
```

Expected for DocumentDB: `me` set, `$clusterTime` absent. (Verified on
MongoDB's side: a standalone mongod has neither, which is why detection
requires `me` before concluding DocumentDB.)

### Q10. The `_id` index walk is a covered scan — **CLUSTER** (5.0.0, 2026-09-01)

> **Confirmed covered.** DocumentDB plans the partition-boundary query as
> `LIMIT_SKIP -> IXONLYSCAN` over `_id_`, where `IXONLYSCAN` is its index-only
> scan — no documents are fetched. The IXONLYSCAN stage reported
> `nReturned: 1000` (skip 999 + 1) in ~2 ms, and LIMIT_SKIP returned 1.
>
> DocumentDB's explain carries no `totalDocsExamined`/`keysExamined` counters
> at all, which is why the first probe run read `undefined`; the stage name is
> the evidence instead. The probe now recognises both shapes.
>
> `TestFindNextIDBoundary` also passes end-to-end against this cluster:
> boundaries evenly spaced, full coverage, 1.8 s.

**Assumption.** The partition-boundary query is served by an index-only scan
that fetches no documents.

**Why it matters.** The walk makes one pass over the `_id` index per
collection. That is acceptable only if it examines zero documents; if
DocumentDB's planner fetches documents instead, partitioning a multi-TB
collection becomes drastically more expensive.

**Confirmed on MongoDB 8.x** (`LIMIT → PROJECTION_COVERED → SKIP → IXSCAN`,
`docsExamined: 0`, `keysExamined == skip + 1`). DocumentDB 5.0+ uses its own
query planner (NQP v2/v3), so this must be re-checked there.

**How to check.**

```js
db.docs.find({_id: {$gt: <someId>}}, {_id: 1})
       .sort({_id: 1}).hint({_id: 1}).skip(999).limit(1)
       .explain("executionStats")
```

Look for zero documents examined and roughly `skip + 1` keys examined.

### Q11. `$collStats` returns usable size, count, and capped — **CLUSTER** (5.0.0, 2026-09-01)

> **Confirmed.** The exact pipeline from `partitions.go` returned
> `count=5000 size=1230000`. Partition sizing works.

**Assumption.** `partitions.GetSizeAndDocumentCount` works, including its
`$group` with `$sum`, `$cond`, and `$first` over `$storageStats`.

**Why it matters.** It supplies the byte size and document count that set
partition granularity. If it fails or returns zeros, we cannot size partitions.

**Source.** AWS lists `$collStats` as supported from 4.0, but explicitly *not*
its `latencyStats`, `recordStats`, and `queryExecStats` options. We use
`storageStats`, which is not called out as unsupported.

**How to check.** Run the exact pipeline from `partitions.go:329` and confirm
non-zero `count` and `size`, and a correct `capped` flag.

### Q12. `listCollections` omits `info.uuid` — **CLUSTER** (5.0.0, 2026-09-01)

> **Confirmed omitted.** The spec carried only `info: {readOnly: false}`. The
> step-6 change from `logger.Fatal()` (an `os.Exit(1)`) to a returned error was
> therefore required, not merely defensive.

**Assumption.** DocumentDB does not return collection UUIDs.

**Why it matters.** `GetCollectionUUID` used to assert a non-nil UUID via
`util.Invariant`, which calls `logger.Fatal()` — an immediate `os.Exit(1)`,
not even a recoverable panic. Step 6 replaced it with an error naming both
causes (a view, or a server that does not report UUIDs). Verified against a
real view on MongoDB.

The answer still matters for diagnosis: if DocumentDB omits `info.uuid`, any
code path needing a UUID now fails with a clear message instead of killing the
process, but it still fails.

**How to check.** `db.runCommand({listCollections: 1})` and look for
`info.uuid` on a collection entry.

### Q13. Capped collections — **CLUSTER** (5.0.0, 2026-09-01) — moot

> **Resolved by non-existence.** DocumentDB refuses to create them:
> `Feature not supported: capped:true`. A DocumentDB source can therefore never
> present a capped collection, so the `$natural`-ordering divergence described
> below cannot arise. The handling in `partition_walk.go` is now unreachable
> defensive code rather than an active trade-off.

**Assumption.** Either DocumentDB has no capped collections, or comparing them
in `_id` order is acceptable.

**Why it matters.** The comparison path sorts capped collections by `$natural`
(`partitions/partition.go:145`), which DocumentDB lacks. The index-walk
partitioner deliberately sets `IsCapped: false` and logs a warning, so capped
collections are compared in `_id` order. That is correct for verifying
equality — natural order only matters for reproducing insertion order — but it
is a deliberate divergence from the MongoDB path.

**How to check.** `db.createCollection("c", {capped: true, size: 100000})` and
see whether DocumentDB accepts it.

### Q14. Change stream pipeline stages are accepted — **CLUSTER** (5.0.0, 2026-09-01) — fixed

> **Confirmed rejected**: opening the change stream failed with
> `Feature not supported: $$REMOVE`.
>
> Fixed by giving DocumentDB its own pipeline that only adds `_docID`. The
> `$$REMOVE` pruning of `updateDescription`/`wallTime`/`documentKey` was a
> bandwidth optimisation, and `ParsedEvent` ignores unknown fields, so dropping
> it costs payload size and nothing else. `$bsonSize` and
> `showSystemEvents`/`showExpandedEvents` were already flavor-gated in step 4.

**Assumption.** The verifier's change stream pipeline
(`change_stream.go:105-162`) survives DocumentDB's restrictions.

**Why it matters.** AWS allows only `$match`, `$project`, `$redact`,
`$addFields`, and `$replaceRoot` in a change stream pipeline. Ours uses
`$match` and `$addFields`, which are allowed, but with `$$REMOVE` to drop
fields and `$bsonSize` to measure the full document. Neither construct is
confirmed inside a DocumentDB change stream.

Note the version gating is actively wrong here: `ClusterHasBSONSize` sees
DocumentDB's reported 5.0 and enables `$bsonSize`, and
`VersionArray[0] >= 6` would enable `showSystemEvents`/`showExpandedEvents` on
a DocumentDB 8.0 cluster. Both need to become flavor-aware.

**How to check.** Open a change stream with the pipeline from
`GetChangeStreamFilter` and confirm it opens and yields correctly shaped
events.

### Q15. `startAfter` is unsupported; `startAtOperationTime` is — **CLUSTER** (5.0.0, 2026-09-01)

> **Confirmed rejected**: `BSON field '$changeStream.startAfter' is an unknown
> field.` The flavor gate added in step 4 is required — without it, every
> resumed change stream against DocumentDB would fail, since DocumentDB reports
> a version that clears `ClusterHasChangeStreamStartAfter`.

**Assumption.** We must use `resumeAfter` or `startAtOperationTime`, never
`startAfter`.

**Why it matters.** `change_stream.go:408` picks `startAfter` for any server
reporting ≥ 4.2, which DocumentDB does. This gate must become flavor-aware.

**Source.** AWS lists resume support as "from a resume token" and "from a
timestamp using `startAtOperationTime` (4.0+)". `startAfter` is absent.

**How to check.** Attempt a `startAfter` watch and record the error.

### Q16. DDL and non-CRUD event types — **CLUSTER** (5.0.0, 2026-09-01)

> **DocumentDB emits only four operation types**: `insert`, `update`,
> `delete`, and `drop`.
>
> Observed by running `createCollection`, `createIndex`, `dropIndex`,
> `updateOne`, `deleteOne`, and `drop()` against a database-level change
> stream. Collection creation and **all index DDL produced no events at all**.
>
> Two consequences:
>
> 1. **Index changes on a DocumentDB source are invisible to the verifier.**
>    On MongoDB, `createIndexes`/`dropIndexes` surface as DDL events and are
>    warned about or rejected. On DocumentDB nothing arrives, so an index
>    created or dropped mid-run passes unnoticed. Generation 0 already compared
>    the indexes, so a later change is simply never seen.
> 2. `drop` is in neither `supportedEventOpTypes` nor `allowedSrcDDLOpTypes`,
>    so dropping a collection aborts the run with `UnknownEventError` even
>    under `--ddlHandling warnMost`. That matches MongoDB's behaviour for
>    `drop` and seems right — but on DocumentDB it is the *only* DDL that can
>    ever abort a run.

**Assumption.** We know which `operationType` values DocumentDB emits.

**Why it matters.** `supportedEventOpTypes` (`change_stream.go:28`) covers
insert/update/replace/delete; anything else is treated as DDL and, under the
default `failAll`, aborts the run. If DocumentDB emits types we don't expect
(or `invalidate` on a dropped collection), verification fails spuriously.

**How to check.** Create/drop collections and indexes while watching, and
record every `operationType` observed.

### Q20. `config.collections` is queryable (or absent, not forbidden) — **CLUSTER** (5.0.0, 2026-09-01)

> **Confirmed readable**, returning an empty result. `getShardKeyFields`
> degrades to "not sharded" as intended, so partitioning is not blocked.

**Assumption.** `sharding.GetShardKey` can run
`config.collections.findOne({_id: "<ns>"})` against DocumentDB and get either
no document or a clean empty result.

**Why it matters.** It runs on every collection during generation 0, before
partitioning (`migration_verifier.go`, `partitionCollection` →
`getShardKeyFields`). It already swallows `ErrNoDocuments` and reports "not
sharded", which is the right answer for DocumentDB. But if DocumentDB instead
*errors* on reading the `config` database — unsupported namespace, or an
authorization failure — partitioning fails for every collection.

**How to check.**

```js
db.getSiblingDB("config").collections.findOne({_id: "somedb.somecoll"})
```

Expected: `null`, not an error.

### Q21. `hello` returns `operationTime` — **CLUSTER** (5.0.0, 2026-09-01)

> **Confirmed.** `hello.operationTime` came back as a Timestamp. The
> writes-off fence in `getDocumentDBServerTime` works as written, and the
> per-batch warning path in `updateTimestamps` should never fire.

**Assumption.** DocumentDB's `hello` reply carries a top-level `operationTime`.

**Why it matters.** `getDocumentDBServerTime` reads it to produce the
writes-off fence, standing in for `appendOplogNote`. If `hello` omits it,
`WritesOff` fails and no verification can reach a final generation. This is
narrower than Q7, which asks only whether *some* command returns it — here we
depend on a specific one.

Note that `GetHelloRaw` previously *required* `operationTime` whenever the
reply advertised `me`, which DocumentDB does. That check is now scoped to
servers that also gossip `$clusterTime`, so it no longer rejects DocumentDB
outright — but it means we no longer learn early whether `hello` carries the
field.

**Partly mitigated in step 6.** `updateTimestamps` used to panic on a nil
`operationTime`; on DocumentDB it now warns once and continues, since those
timestamps only feed lag reporting. So a missing `operationTime` degrades
progress display rather than crashing — but `WritesOff` still fails outright,
because the fence genuinely needs a server timestamp.

**How to check.**

```js
db.adminCommand({hello: 1}).operationTime   // expect a Timestamp, not undefined
```

If absent, find a command that does return one (`dbStats`, `find`, …) and use
that in `getDocumentDBServerTime` instead.

### Q22. The quiescence drain terminates correctly — **CLUSTER** (5.0.0, 2026-09-01)

> **Confirmed end to end.** Triggering `/api/v1/writesOff` mid-run produced:
>
> ```
> Read DocumentDB server time. operationTime={T:1788269756}
> source change stream reader draining to quiescence. requiredIdlePolls=3
> source change stream reader has drained past the writesOff timestamp.
>     idlePolls=3 serverTimestamp={T:1788269760} writesOffTimestamp={T:1788269756}
> Final generation done. generation=37
> ```
>
> Both termination conditions fired as designed: the server clock passed the
> fence (…760 > …756) *and* three consecutive idle polls elapsed, taking ~3 s
> at the 1 s await time. `getDocumentDBServerTime` supplied the fence in place
> of `appendOplogNote`.
>
> The destination, being MongoDB, took the ordinary resume-token path in the
> same run, so both drains were exercised together.
>
> **Superseded in part by Q30**: this confirmation was run against a quiesced
> source. With writes continuing past writes-off, the idle rule alone hangs
> forever. The drain now also terminates on consuming an event past the fence.
>
> Still untested: a drain racing a long-running `updateMany`, which AWS
> documents as able to stall change stream writes and could in principle
> produce a deceptive lull.

**Assumption.** After writes stop, `operationTime` advances past the fence and
the change stream returns `docDBDrainIdlePolls` consecutive empty batches
within a reasonable time.

**Why it matters.** `drainDocumentDBUntilQuiesced` replaces MongoDB's exact
resume-token comparison, which DocumentDB cannot support. It infers "no more
events" from "no events lately", so it is inherently weaker. Two failure
modes: if `operationTime` never advances while idle, the drain hangs; if
DocumentDB can deliver an event *after* several empty polls, the drain could
end early and drop rechecks.

Recall that AWS documents a long-running `updateMany`/`deleteMany` as able to
"temporarily stall the writing of change streams events" — exactly the shape
that could produce a deceptive lull.

**How to check.** Run a verification under write load, stop the writes, call
`WritesOff`, and confirm the drain ends promptly and that the event count it
saw matches the writes performed. Repeat with a large `updateMany` in flight
at writes-off time.

### Q23. Index specs carry a `ns` field — **CLUSTER** (5.0.0, 2026-09-01) — fixed

> **Resolved, and worse than first recorded.** The full divergence is three
> fields, not one:
>
> | field | DocumentDB | MongoDB 7.0 |
> | --- | --- | --- |
> | `v` | 4 | 2 |
> | `ns` | present | absent |
> | `2dsphereIndexVersion` | 1 | 3 |
>
> `v: 4` does not merely mismatch — the index comparator **aborts the entire
> verification** with "index has an unexpected `v` value: 4".
>
> `normalizeIndexSpecForDocumentDB` drops `ns` and `2dsphereIndexVersion` and
> rewrites `v` to 2. `v` cannot be dropped: the comparator requires the field
> and fails with "extracting `v` from index spec: element not found".
>
> Everything defining an index — key, unique, sparse, partialFilterExpression,
> expireAfterSeconds — matched exactly across all 10 index types tested.

> **Confirmed divergence.** DocumentDB reports index specs as
> `{v: 2, key: {_id: 1}, name: "_id_", ns: "db.coll"}`. MongoDB **removed**
> `ns` from index specs in 4.4, so a modern destination will not have it.

`verifyIndexes` compares specs field by field via
`index.DescribeSpecDifferences`, so **every index on every collection will
report a spurious mismatch** on the `ns` field alone. Worse, `ns` holds the
source namespace, so it cannot match a remapped destination even in principle.

Needs either a DocumentDB-aware normalisation that strips `ns` before
comparison, or a default `--indexSpecIgnore` entry for DocumentDB sources.

### Q24. Collection options diverge structurally — **CLUSTER** (5.0.0, 2026-09-01) — fixed

> **Far more severe than first recorded.** This was filed as a false-positive
> noise problem. In fact it **silently skipped all document verification**.
>
> DocumentDB states `capped: false`; MongoDB omits the field. The inequality
> made `canCompareData` false, which hits an early `return nil` before
> partitioning — so generation 0 completed "successfully" having compared zero
> documents, while reporting only a metadata mismatch.
>
> Fixed two ways: `isCappedOption` compares capped-ness semantically (absent ==
> false), and `normalizeCollectionOptionsForDocumentDB` drops `storageEngine`
> and `autoIndexId`.

> **Confirmed divergence.** DocumentDB reported collection options as
> `{autoIndexId: true, capped: false, storageEngine: {documentDB: {compression: {enable: false}}}}`.

`compareCollectionSpecifications` does a `bytes.Equal` on the raw options and
then a detailed field comparison, so **every collection will report a metadata
mismatch**: `storageEngine.documentDB` is DocumentDB-specific and has no
MongoDB counterpart, and `autoIndexId` is long removed from MongoDB.

Needs a DocumentDB-aware normalisation of the options document before
comparison — at minimum dropping `storageEngine` and `autoIndexId`, and
treating `capped: false` as equivalent to absent.

Note this is a false-*positive* class of failure: it reports mismatches that
are not real, rather than hiding real ones. It makes output unusable, but it
does not threaten correctness the way Q4 does.

### Q25. The Go driver ignores `tlsAllowInvalidHostnames` — **CLUSTER** (5.0.0, 2026-09-01) — setup gotcha

> **Confirmed.** The MongoDB Go driver v2 parses only `tlsInsecure`
> (and its `sslInsecure` alias); it has no `tlsAllowInvalidHostnames` option.
> Passing the mongosh-style URI to the verifier fails after a 60 s server-
> selection timeout with:
>
> `tls: failed to verify certificate: x509: certificate is valid for
> <cluster endpoints>, not localhost`

So a tunnelled URI that works in `mongosh` does **not** work with the verifier.
Two ways to fix it, in order of preference:

1. **Map the hostname.** Add the cluster endpoint to `/etc/hosts` pointing at
   `127.0.0.1` and use the real hostname in the URI. Certificate validation
   then passes completely, with no TLS relaxation. The port may still differ
   from 27017; certificates carry no port.
2. **`tlsInsecure=true`.** Works, but disables certificate-chain *and*
   hostname verification. Acceptable for local testing over an SSH tunnel that
   already authenticates the endpoint; not something to document as the normal
   path.

### Q26. Views are unsupported — **CLUSTER** (5.0.0, 2026-09-01)

> **Confirmed.** `createView` fails with
> `Field 'pipeline' is currently not supported`.

`compareCollectionSpecifications` branches on `srcSpec.Type == "view"`, which a
DocumentDB source can never produce. Like Q13 (capped collections), this is a
whole class of handling that cannot be exercised from a DocumentDB source.

### Q27. Retryable writes are unsupported — **CLUSTER** (5.0.0, 2026-09-01)

> **Confirmed.** Opening a change stream and writing on a `retryWrites=true`
> connection fails with `Retryable writes are not supported`. AWS's own sample
> code sets `retryWrites=false`.

**No verifier change is needed today**, and the reason is worth recording: the
verifier never writes to the source or the destination. It reads both and
writes only to the metadata cluster, which Gate D already requires to be
MongoDB. The one source write it used to perform, `appendOplogNote`, is now
MongoDB-only.

It does matter for anything else pointed at the cluster: add
`retryWrites=false` to `mongosh` and to any tooling that writes. If the
verifier ever gains a source or destination write, this becomes a real
requirement.

### Q28. `$expr` partition filters work — **CLUSTER** (5.0.0, 2026-09-01) — no change needed

> **Recorded because it was nearly "fixed" in the wrong direction.**
>
> When a run compared zero documents, `$expr` was the prime suspect:
> `FilterIdBounds` uses `$expr` when the version is >= 5, and DocumentDB
> reports 5. Testing disproved it — `$expr` with `$literal` MinKey/MaxKey
> bounds returns all 50,000 documents, and `$expr` combined with `hint` and
> `sort` behaves correctly too.
>
> The zero-document reading was a measurement error: the metadata query used
> field names that do not exist (`source_documents_compared` rather than
> `found_source_documents_count`), so `$sum` returned 0.
>
> Critically, the "fix" would have broken things. The alternative path,
> `getExplicitTypeCheckPredicates`, emits `{_id: {$type: [...]}}`, and
> DocumentDB rejects that: `Feature not supported: array for $type filter`.
> **`FilterIdBounds` must keep using the `$expr` path for DocumentDB.**

### Q29. Sessions must disable causal consistency — **CLUSTER** (5.0.0, 2026-09-01) — fixed

> **Found by the failover test; it crashed the verifier.**
>
> ```
> Non-retryable error: change stream iteration failed:
>   Feature not supported: 'causal consistency'   (code 303)
> ```
>
> The Go driver creates causally-consistent sessions by default. This does not
> fail immediately, which is what makes it dangerous: a session's first
> operation carries no `afterClusterTime`, so opening a change stream succeeds
> and the run looks healthy indefinitely — 32 generations passed here. Only
> once the session has recorded an `operationTime` does the driver begin
> sending `afterClusterTime`, so the failure appears on the first **resume** —
> in practice, during a failover.
>
> `sessionOptsForFlavor` now sets `SetCausalConsistency(false)` for DocumentDB,
> applied at all four `StartSession` sites. The two in `compare.go` had
> survived only because each session is short-lived enough never to reach a
> second operation; a retry inside one would have hit the same wall.
>
> Nothing is lost by disabling it: the verifier never relied on causal
> consistency against DocumentDB. Read ordering comes from opening the change
> reader before any scan and trusting the primary's read-after-write guarantee
> (Q1).

### Q30. The drain must not require idleness — **CLUSTER** (5.0.0, 2026-09-01) — fixed

> **The step-5 drain hung when writes continued past writes-off.**
>
> Q22's original confirmation was run against a quiesced source, where the
> quiescence rule works. With a writer still running, the stream never goes
> idle, so the drain waited indefinitely — observed hanging for over three
> minutes until the writer was killed, at which point it completed normally.
>
> MongoDB's drain does not have this problem: it stops once the resume token
> passes the fence, regardless of ongoing writes.
>
> `drainDocumentDBUntilQuiesced` now terminates on either condition:
>
> 1. **An event at or past the fence has been consumed.** The stream delivers
>    in order, so everything before the fence is necessarily consumed too.
>    This is the same reasoning MongoDB uses with the resume token.
> 2. The original idle rule, for a genuinely quiet stream where no event will
>    ever cross the fence.
>
> Re-tested with a writer running through writes-off: the drain now terminates
> in the same second via condition 1.

---

## Priority 3 — nice to confirm

### Q17. `buildInfo.versionArray` shape — **CLUSTER** (5.0.0, 2026-09-01)

> **Confirmed.** `versionArray=[5,0,0,0]`, `version=5.0.0`. Four elements;
> `GetVersionArray` copies into a `[3]int` and so takes `[5,0,0]`, which is
> correct. Note this is exactly the misleading number that made the flavor axis
> necessary — it clears every MongoDB 5.0 feature gate.

`mmongo.GetVersionArray` requires a `versionArray` field. Confirm DocumentDB
returns one and note what it reports per engine version, since our version
gates read it.

### Q18. Retention is not readable over the wire — **CLUSTER** (5.0.0, 2026-09-01) — confirmed unreadable

> **Confirmed.** `getParameter` is not implemented at all:
> `Feature not supported: getParameter`, for `{getParameter: "*"}` and for
> named lookups of `change_stream_log_retention_duration` in either
> snake_case or camelCase.
>
> So `change_stream_log_retention_duration` genuinely cannot be read from a
> MongoDB connection, and `warnDocumentDBChangeStreamRetention` is right to
> emit an unconditional advisory rather than pretend to check.

`change_stream_log_retention_duration` lives in a DB cluster parameter group.
We currently emit an unconditional advisory
(`warnDocumentDBChangeStreamRetention`) because we found no way to read it from
a MongoDB connection. If one exists (some `getParameter` variant), we could
turn the advisory into a real check.

### Q19. Index specification comparison — superseded by Q23

`verifyIndexes` compares index specs field by field between clusters.
DocumentDB may report different or additional spec fields than MongoDB for
equivalent indexes, producing spurious mismatches. If so, the existing
`--indexSpecIgnore` tolerances may need DocumentDB-specific defaults.

---

## Confirmed and closed

Nothing yet — no item has reached **CLUSTER**.

When closing an item, record the DocumentDB engine version and the date, and
move it here with the observed output.
