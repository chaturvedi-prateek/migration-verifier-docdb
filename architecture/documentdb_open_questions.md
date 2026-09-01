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

### Q1. Primary reads are read-after-write consistent — **DOC**

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

### Q2. Change events always carry `clusterTime` — **DOC**

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

### Q3. Resume token `_data` is not a MongoDB KeyString — **DOC**

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

### Q4. Resume-token expiry is detectable — **OPEN**

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

### Q5. `$listChangeStreams` works via the driver's `Database.Aggregate` — **OPEN**

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

### Q6. `readConcern: {level: "majority"}` is accepted — **OPEN**

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

### Q7. `operationTime` is present in command responses — **OPEN**

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

### Q8. Change stream survives a failover without a gap — **OPEN**

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

### Q9. Flavor detection is correct on a real cluster — **OPEN**

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

### Q10. The `_id` index walk is a covered scan — **OPEN**

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

### Q11. `$collStats` returns usable size, count, and capped — **OPEN**

**Assumption.** `partitions.GetSizeAndDocumentCount` works, including its
`$group` with `$sum`, `$cond`, and `$first` over `$storageStats`.

**Why it matters.** It supplies the byte size and document count that set
partition granularity. If it fails or returns zeros, we cannot size partitions.

**Source.** AWS lists `$collStats` as supported from 4.0, but explicitly *not*
its `latencyStats`, `recordStats`, and `queryExecStats` options. We use
`storageStats`, which is not called out as unsupported.

**How to check.** Run the exact pipeline from `partitions.go:329` and confirm
non-zero `count` and `size`, and a correct `capped` flag.

### Q12. `listCollections` omits `info.uuid` — **OPEN**

**Assumption.** DocumentDB does not return collection UUIDs.

**Why it matters.** `uuidutil/get_uuid.go:66` has a hard invariant that the
UUID is non-nil and **panics** otherwise, taking down the process. Scheduled
for step 6.

**How to check.** `db.runCommand({listCollections: 1})` and look for
`info.uuid` on a collection entry.

### Q13. Capped collections — **OPEN**

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

### Q14. Change stream pipeline stages are accepted — **OPEN**

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

### Q15. `startAfter` is unsupported; `startAtOperationTime` is — **DOC**

**Assumption.** We must use `resumeAfter` or `startAtOperationTime`, never
`startAfter`.

**Why it matters.** `change_stream.go:408` picks `startAfter` for any server
reporting ≥ 4.2, which DocumentDB does. This gate must become flavor-aware.

**Source.** AWS lists resume support as "from a resume token" and "from a
timestamp using `startAtOperationTime` (4.0+)". `startAfter` is absent.

**How to check.** Attempt a `startAfter` watch and record the error.

### Q16. DDL and non-CRUD event types — **OPEN**

**Assumption.** We know which `operationType` values DocumentDB emits.

**Why it matters.** `supportedEventOpTypes` (`change_stream.go:28`) covers
insert/update/replace/delete; anything else is treated as DDL and, under the
default `failAll`, aborts the run. If DocumentDB emits types we don't expect
(or `invalidate` on a dropped collection), verification fails spuriously.

**How to check.** Create/drop collections and indexes while watching, and
record every `operationType` observed.

### Q20. `config.collections` is queryable (or absent, not forbidden) — **OPEN**

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

### Q21. `hello` returns `operationTime` — **OPEN**

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

**How to check.**

```js
db.adminCommand({hello: 1}).operationTime   // expect a Timestamp, not undefined
```

If absent, find a command that does return one (`dbStats`, `find`, …) and use
that in `getDocumentDBServerTime` instead.

### Q22. The quiescence drain terminates correctly — **OPEN**

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

---

## Priority 3 — nice to confirm

### Q17. `buildInfo.versionArray` shape — **OPEN**

`mmongo.GetVersionArray` requires a `versionArray` field. Confirm DocumentDB
returns one and note what it reports per engine version, since our version
gates read it.

### Q18. Retention is not readable over the wire — **OPEN**

`change_stream_log_retention_duration` lives in a DB cluster parameter group.
We currently emit an unconditional advisory
(`warnDocumentDBChangeStreamRetention`) because we found no way to read it from
a MongoDB connection. If one exists (some `getParameter` variant), we could
turn the advisory into a real check.

### Q19. Index specification comparison — **OPEN**

`verifyIndexes` compares index specs field by field between clusters.
DocumentDB may report different or additional spec fields than MongoDB for
equivalent indexes, producing spurious mismatches. If so, the existing
`--indexSpecIgnore` tolerances may need DocumentDB-specific defaults.

---

## Confirmed and closed

Nothing yet — no item has reached **CLUSTER**.

When closing an item, record the DocumentDB engine version and the date, and
move it here with the observed output.
