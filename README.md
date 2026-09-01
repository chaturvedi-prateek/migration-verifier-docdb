# Verify Migrations!

_If verifying a migration done via [mongosync](https://www.mongodb.com/docs/cluster-to-cluster-sync/current/), please check if it is possible to use the
[embedded verifier](https://www.mongodb.com/docs/cluster-to-cluster-sync/current/reference/verification/embedded/#std-label-c2c-embedded-verifier) as that is the preferred approach for verification._

# Quick Start

Download the verifier’s latest release:
```
curl -sSL https://raw.githubusercontent.com/mongodb-labs/migration-verifier/refs/heads/main/download_latest.sh | sh
```
(Alternatively, you can check out this repository then `./build.sh` to build from source.)

Finally, run verification:
```
./migration_verifier \
    --srcURI mongodb://your.source.cluster \
    --dstURI mongodb://your.destination.cluster \
    --serverPort 0 \
    --verifyAll \
    --start
```
The above will stream verification logs to standard output. Once writes stop,
wait for change stream lag to hit 0. The log will report either the found
mismatches or a confirmation of exact match between the clusters.

# More Details

To see all options:


```
./migration_verifier --help
```


To check all namespaces:


```
./migration_verifier --srcURI mongodb://127.0.0.1:27002 --dstURI mongodb://127.0.0.1:27003 --verifyAll
```


To check only specific namespaces:


```
./migration_verifier --srcURI mongodb://127.0.0.1:27002 --dstURI mongodb://127.0.0.1:27003 --srcNamespace foo.bar --dstNamespace foo.bar --srcNamespace foo.yar --dstNamespace foo.yar --srcNamespace mixed.namespaces --dstNamespace can.work
```


Note: that this will check from` foo.bar <-> foo.bar, foo.yar <-> foo.yar, `and` mixed.namespaces <-> can.work`

For checking namespaces with differing names between source and destination, the namespaces must be explicitly enumerated on the command line with the` --srcNamespace and --dstNamespace` flags. Names in the same position are considered to be the same:


```
migration-verifier … --srcNamespace foo.bar --srcNamespace foo.baz --dstNamespace foo.bar1 --dstNamespace bar.bar2 
```


will result in the mapping `foo.bar <-> foo.bar1, foo.baz <-> bar.bar2`

By default, the verifier will read from the primary node.  This can be changed with option “`--readPreference <preference>`” where `<preference>` can be “`primary`” (same as default), “`secondary`”, “`primaryPreferred`”, “`secondaryPreferred`”, or “`nearest`”.

To set a port, use `--serverPort <port number>`. The default is 27020. Note that migration-verifier listens on all available network interfaces, not just on `localhost`.

If you give 0 as the port, a random ephemeral port will be chosen. The log will show the chosen port, and you may also query the OS to learn it (e.g., `lsof -a -iTCP -sTCP:LISTEN -p <pid>`).

## Using a configuration file

To load configuration options from a YAML configuration file, use the `--configFile` parameter.

For example, you can specify `srcURI`, `dstURI`, and `metaURI` parameters thus:
```
---
srcURI: mongodb://localhost:28010
dstURI: mongodb://localhost:28011
metaURI: mongodb://localhost:28012
```
## Metadata considerations

By default, the verifier stores its metadata on the destination cluster. This works well in most migrations. It does require a destination that can handle
both the migration *and* the verification workloads concurrently.

If this combined workload overpowers the destination, put the
verification’s metadata on a different cluster. To do this, give that cluster’s
connection string in the `--metaURI` parameter.

## Send the Verifier Process Commands:

1. After launching the verifier (see above), you can send it requests to get it to start verifying. If you don’t pass the `--start` parameter, verification is started by using the `check` command. An [optional `filter` parameter](#document-filtering) can be passed within the `check` request body to only check documents within that filter. The verification process will keep running until you tell the verifier to stop. It will keep track of the inconsistencies it has found and will keep checking those inconsistencies hoping that eventually they will resolve.

```
curl -H "Content-Type: application/json" -d '{}' http://127.0.0.1:27020/api/v1/check
```


2. Once writes on the source cluster have stopped, you can tell the verifier that writes have stopped. (You can see the state of mongosync’s replication by hitting mongosync’s `progress` endpoint and checking that the state is `COMMITTED`. See the documentation [here](https://www.mongodb.com/docs/cluster-to-cluster-sync/current/reference/api/progress/#response)). \
The verifier will now check to completion to make sure that there are no inconsistencies. The command you need to send the verifier here is `writesOff`. The command doesn’t block. This means that you will have to poll the verifier, or watch its logs, to see the status of the verification (see `progress`).

```
curl -H "Content-Type: application/json" -d '{}' http://127.0.0.1:27020/api/v1/writesOff
```


3. You can poll the verification’s status by hitting the `progress` endpoint. In particular, the `phase` should reveal whether the verifier is done verifying. Once the `phase` is `idle`, the verification has completed. Once the `phase` is `idle`, the `error` field should be null, and the `failedTasks` field should be 0, if the verification was successful. A non-null `error` field indicates that the verifier itself ran into an error. If `failedTasks` is not 0, the verifier found an inconsistency. The verifier’s logs should detail the inconsistencies.

```
curl http://127.0.0.1:27020/api/v1/progress
```

### `/progress` API Response

In the below a “timestamp” is an object with `T` and `I` unsigned integers.
These represent a logical time in MongoDB’s replication protocol.

- `progress`
  - `phase` (string): either `idle`, `check`, or `recheck`
  - `generation` (unsigned integer)
  - `generationStats`
    - `docsCompared` (unsigned)
    - `totalDocs` (unsigned)
    - `srcBytesCompared` (unsigned)
    - `totalSrcBytes` (unsigned, only present in `check` phase)
    - `totalNamespaces` (unsigned)
  - `gen0Stats` (absent during generation 0; present from generation 1 onward): final
    doc/byte counts from the completed initial check
    - `docsCompared` (unsigned)
    - `totalDocs` (unsigned)
    - `srcBytesCompared` (unsigned)
    - `totalSrcBytes` (unsigned)
    - `totalNamespaces` (unsigned)
  - `recentRecheckSecs` (array of recent recheck generations’ durations)
  - `srcChangeStats`
    - `eventsPerSecond` (unsigned)
    - `lagSecs` (unsigned)
    - `bufferSaturation` (fraction)
    - `eventCounts` (totals since the migration’s start)
      - `insert`
      - `update`
      - `replace`
      - `delete`
  - `dstChangeStats` (same fields as `srcChangeStats`)
  - `srcLastRecheckedTS` (see below)
  - `totalRechecksDone` (unsigned, total number of times, across all generations, a document has been rechecked)
  - `longestMismatch` (See `/docMismatches` below for format.)
  - `error` (string, optional)
  - `verificationStatus` (tasks for the current generation)
    - `totalTasks` (unsigned integer)
    - `addedTasks` (unsigned integer, unstarted tasks)
    - `processingTasks` (unsigned integer, in-progress tasks)
    - `failedTasks` (unsigned integer, tasks that found a document mismatch)
    - `completedTasks` (unsigned integer, tasks that found no problems)
    - `metadataMismatchTasks` (unsigned integer, tasks that found a collection metadata mismatch)

NOTE: Byte-total figures always reflect true document size. In hashed-verification mode the
number of bytes that Verifier compares is generally much smaller; there currently is no
metric to track that.

This is sample output:
```
{
  "progress": {
    "phase": "recheck",
    "generation": 2,
    "generationStats": {
      "docsCompared": 0,
      "totalDocs": 2040204,
      "srcBytesCompared": 0
    },
    "recentRecheckSecs": [
        20.3,
        10.254,
        23.2
    ],
    "error": null,
    "verificationStatus": {
      "totalTasks": 204,
      "addedTasks": 204,
      "processingTasks": 0,
      "failedTasks": 0,
      "completedTasks": 0,
      "metadataMismatchTasks": 0
    },
    "srcLastRecheckedTS": {
      "$timestamp": {
        "t": 1773253202,
        "i": 2186
      }
    },
    "dstLastRecheckedTS": {
      "$timestamp": {
        "t": 1773253202,
        "i": 10030
      }
    },
    "srcChangeStats": {
      "eventsPerSecond": 4881.42374871582,
      "lagSecs": 0,
      "bufferSaturation": 0.01
    },
    "dstChangeStats": {
      "eventsPerSecond": 32803.89071205276,
      "lagSecs": 0,
      "bufferSaturation": 0.95
    },
    "docsComparedPerSecond": 75061.86338662436,
    "srcBytesComparedPerSecond": 43954387.31067323
  }
}
```

#### Last recheck timestamps

The `srcLastRecheckedTS` and `dstLastRecheckedTS` fields indicate the
oplog timestamp of the last write that the verifier has rechecked.

Consider a write on the source at oplog timestamp {123, 234}. Immediately
after the write happens, the verifier will not have checked whether the
write was replicated. By checking whether `srcLastRecheckedTS` has met or
exceeded that value, you can know when the verifier has done a recheck
for that write.

In a migration, once you have quiesced writes on your source cluster,
monitor the replicator & correlate its last-replicated optime with
`srcLastRecheckedTS`. This will tell you when it’s safe to proceed with
cutover.

### `/summary` API Response

The `/summary` endpoint returns a compact, aggregated digest of the
verifier’s state. It is intended for periodic, low-volume polling
(e.g., a status display) and rolls the per-mismatch detail returned by
`/docMismatches` and `/nsMismatches` into counts.

```
curl http://127.0.0.1:27020/api/v1/summary
```

The response has the following fields:

- `estCheckSecsRemaining` (number or null): estimated seconds until the
  initial check (generation 0) finishes, computed from the current
  source bytes-per-second rate. Null when the verifier is not in the
  `check` phase or has not yet measured a rate; `0` once the check
  is complete.
- `recentRecheckSecs` (array of numbers, optional): durations of recent
  recheck generations. (Same as `/progress`.)
- `checkStats` (object, optional): final doc/byte counts from the
  initial check. During generation 0 it reports the in-progress
  stats; afterwards it reports the cached final gen-0 stats. Absent
  before any data is available.
  - `docsCompared` (unsigned)
  - `totalDocs` (unsigned)
  - `srcBytesCompared` (unsigned)
  - `totalSrcBytes` (unsigned)
  - `totalNamespaces` (unsigned)
- `nsMismatches` (array): namespace-level mismatches in the same
  format as the `/nsMismatches` documents. (See
  [Namespaces](#namespaces) below.)
- `docMismatches` (object): document-mismatch tallies. The per-document
  detail is available via `/docMismatches`.
  - `total` (unsigned): total tracked document mismatches.
  - `byType` (object): counts keyed by mismatch type (`missingOnDst`,
    `extraOnDst`, `content`).
  - `byNamespace` (object): counts keyed by namespace.
- `totalRechecksDone` (unsigned): same as in `/progress`.
- `srcChangeEvents` (object): cumulative change-event counts on the
  source since the verifier started.
  - `insert`
  - `update`
  - `replace`
  - `delete`
- `dstChangeEvents` (object): same shape as `srcChangeEvents`, for the
  destination.
- `notes` (array of strings, optional): human-readable notes describing
  caveats applied to this response (e.g., that document mismatches
  were filtered by `minDurationSecs`).

This is sample output:
```
{
  "estCheckSecsRemaining": null,
  "recentRecheckSecs": [
    20.3,
    10.254,
    23.2
  ],
  "checkStats": {
    "docsCompared": 2040204,
    "totalDocs": 2040204,
    "srcBytesCompared": 1073741824,
    "totalSrcBytes": 1073741824,
    "totalNamespaces": 12
  },
  "nsMismatches": [],
  "docMismatches": {
    "total": 2,
    "byType": {
      "content": 1,
      "missingOnDst": 1
    },
    "byNamespace": {
      "test.coll": 2
    }
  },
  "totalRechecksDone": 4823,
  "srcChangeEvents": {
    "insert": 1000,
    "update": 250,
    "replace": 0,
    "delete": 5
  },
  "dstChangeEvents": {
    "insert": 1000,
    "update": 250,
    "replace": 0,
    "delete": 5
  }
}
```

#### Limiting Document Mismatch Counts

As with `/docMismatches`, you can pass a `minDurationSecs` parameter to
restrict the document mismatches counted in `docMismatches` to those
seen for at least that many seconds:

```
curl 'http://127.0.0.1:27020/api/v1/summary?minDurationSecs=60'
```

This filter only affects `docMismatches`; namespace mismatches are
unaffected. When set, a corresponding entry is added to `notes`.

# CLI Options

| Flag                                    | Description                                                                                                                                                                                 |
|-----------------------------------------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `--configFile <value>`                  | path to an optional YAML config file                                                                                                                                                        |
| `--srcURI <URI>`                        | source Host URI for migration verification (required)                                                                                                           |
| `--dstURI <URI>`                        | destination Host URI for migration verification (required)                                                                                                      |
| `--metaURI <URI>`                       | host URI for storing migration verification metadata (default: same as `--dstURI`)                                                                                                 |
| `--serverPort <port>`                   | port for the control web server (default: 27020)                                                                                                                                            |
| `--logPath <path>`                      | logging file path (default: "stdout")                                                                                                                                                       |
| `--numWorkers <number>`                 | number of worker threads to use for verification (default: 10)                                                                                                                              |
| `--generationPauseDelay <milliseconds>` | milliseconds to wait between generations of rechecking, allowing for more time to turn off writes (default: 1000)                                                                           |
| `--workerSleepDelay <milliseconds>`     | milliseconds workers sleep while waiting for work (default: 1000)                                                                                                                           |
| `--srcNamespace <namespaces>`           | source namespaces to check                                                                                                                                                                  |
| `--dstNamespace <namespaces>`           | destination namespaces to check                                                                                                                                                             |
| `--metaDBName <name>`                   | name of the database in which to store verification metadata (default: `__mdb_internal_migration_verifier`)                                                                                   |
| `--docCompareMethod`                    | How to compare documents. See below for details.                                                                                                                                        |
| `--partitioningScheme`                  | How to partition collections. See below for details.                                                                                                                                        |
| `--srcChangeReader`                     | How to read changes from the source. See below for details.             |
| `--srcFlavor`                           | Source server implementation: `auto` (default), `mongodb`, or `documentdb`. Detection is normally reliable; set this only to override it. See below.             |
| `--dstChangeReader`                     | How to read changes from the destination. See below for details.        |
| `--start`                               | Start checking documents right away rather than waiting for a `/check` API request. |
| `--verifyAll`                           | If set, verify all user namespaces                                                                                                                                                          |
| `--clean`                               | If set, drop all previous verification metadata before starting                                                                                                                             |
| `--ddlHandling`                         | Either `failAll` (default) or `warnMost`. Under `failAll` any DDL change events will crash the verifier. `warnMost` makes the verifier instead show a warning and skip most DDL events. (In this case you **MUST** manually confirm that the change is replicated to the destination.) Drop and rename changes still trigger a crash under `warnMost`. |
| `--readPreference <value>`              | Read preference for reading data from clusters. May be 'primary', 'secondary', 'primaryPreferred', 'secondaryPreferred', or 'nearest' (default: "primary")                                  |
| `--partitionSizeMB <Megabytes>`         | Megabytes to use for a partition.  Change only for debugging. 0 means use partitioner default. (default: 0)                                                                                 |
| `--logLevel`                               | Set the logging to `info`, `debug`, or `trace` level.                                                                                                                                                                       |
| `--indexSpecIgnore` | Makes logs ignore index mismatches around the given index-specification fields. Allowed values are: [`expireAfterSeconds`, `unique`] (comma separated) |
| `--checkOnly`                           | Do not run the webserver or recheck, just run the check (for debugging)                                                                                                                     |
| `--failureDisplaySize <value>`          | Number of failures to display. Will display all failures if the number doesn’t exceed this limit by 25% (default: 20)                                                                       |
| `--ignoreReadConcern`                   | Use connection-default read concerns rather than setting majority read concern. This option may degrade consistency, so only enable it if majority read concern (the default) doesn’t work. |
| `--help`, `-h`                          | show help                                                                                                                                                                                   |

# Investigation of Mismatches

## Documents

The following API command:
```
curl http://localhost:27020/api/v1/docMismatches
```
… will return a stream of newline-delimited JSON documents that describe
currently-tracked mismatches.

Each mismatch document looks like:
- `durationSecs`: the # of seconds between when the mismatch was first
  seen and the most recent time it was seen
- `type`: one of `missingOnDst`, `extraOnDst`, or `content`
- `namespace`
- `_id` (relaxed ext JSON)
- `field`: the field in the document that mismatched (only set with
  `content`-type mismatches when not using hashed comparison)
- `detail`: human-readable description of the mismatch (only set with
  `content`-type mismatches)

The results are returned sorted by `durationSecs`, descending.

Example output:
```
{
    "type": "missingOnDst",
    "namespace": "test.coll",
    "_id": 111,
    "durationSecs": 8.454
}
{
    "type": "content",
    "namespace": "test.coll",
    "_id": 222,
    "field": "name",
    "detail": "Mismatch",
    "durationSecs": 8.454
}
{
    "type": "extraOnDst",
    "namespace": "test.coll",
    "_id": 333,
    "durationSecs": 8.454
}
```
During generation 0, this API command returns mismatches for generation 0.
Thereafter it returns mismatches for the _prior_ generation.

### Limiting Results

You can optionally send a `minDurationSecs` parameter to limit results by
a minimum duration. For example, the following suppresses all mismatches
that have been seen for less than 1 minute:
```
curl 'http://localhost:27020/api/v1/docMismatches?minDurationSecs=60'
```

## Namespaces

The following API command:
```
curl http://localhost:27020/api/v1/nsMismatches
```
… is like `/docMismatches` but returns namespace-level mismatches.

Each mismatch document looks like:
- `type`: As in `/docMismatches`.
- `namespace`
- `aspect`: The mismatching characteristic of the namespace.
- `component`: Depends on `aspect`. See below.
- `detail`: As in `/docMismatches`.

`aspect` can be any of the following:

- `exist`: The namespace is either missing or extra on the destination.
- `type`: The namespace is of different types on source & destination.
- `index`: An index is mismatched, missing, or extra. `component` is the
  index’s name.
- `spec`: An element of the collection’s specification mismatches.
  `component` names the part of the spec that differs.
- `shard key`: The collection’s shard key differs between source and
  destination.
- `readOnly`: The collection’s read-only flag differs between source and
  destination.

No sort order is defined.

Sample output:
```
{
  "type": "missingOnDst",
  "namespace": "test.missingColl",
  "aspect": "exist"
}
{
  "type": "content",
  "namespace": "test.indexesColl",
  "aspect": "index",
  "component": "foo_1",
  "detail": "{\"op\":\"remove\",\"path\":\"/collation\"}"
}
{
  "type": "missingOnDst",
  "namespace": "test.lostCapped",
  "aspect": "spec",
  "component": "options.capped"
}
{
  "type": "missingOnDst",
  "namespace": "test.lostCapped",
  "aspect": "spec",
  "component": "options.size"
}
```

### Limiting Index Mismatches

You can optionally send an `indexSpecIgnore` parameter whose value is a
comma-delimited list of any (or all) of:

- `expireAfterSeconds`
- `unique`

By submitting this parameter you will cause the API to discard any
mismatches that concern the relevant fields in an index specification.

# Tests

This project’s tests run as normal Go tests, to, with `go test`.

`IntegrationTestSuite`'s tests require external clusters. You must provision these yourself.
(See the project’s GitHub CI setup for one way to simplify it.) Once provisioned, set the relevant
connection strings in the following environment variables:

- MVTEST_SRC
- MVTEST_DST
- MVTEST_META

# How the Verifier Works

The migration-verifier has two steps:

1. The initial check
    1. The verifier partitions up the data into 400MB (configurable) chunks and spins up many worker goroutines (threads) to read from both the source and destination.
    2. The verifier compares the documents on the source and destination by bytes and if they are different, it then checks field by field in case the field ordering has changed (since field ordering isn't required to be the same for the migration to be a success)

2. Iterative checking
    1. Since writes are coming in while the verification is happening, the verifier could both miss problematic changes made by the migration tool and could have temporary inconsistencies  that will clean themselves up later
    2. Before starting the initial check, the verifier starts a change stream on the source and keeps track of every document that is modified on the source
    3. In addition, the verifier keeps track of any documents that fail a check
    4. The verifier runs rounds of checks continuously until it is told that writes are off, fetching the documents stored from the change stream and that were inconsistent in the previous checking rounds, from both the source and destination and rechecking them. Once again, violations are written down for future checking rounds
    5. Every document to check is written with a generation number. A checking round checks documents for a specific generation. When a check round begins, we start writing new documents with a new generation number
    6. The verifier fetches all collection/index/view information on the source and destination and confirms they are identical in every generation. This is duplicated work, but it's fast and convenient for the code.

# Document Filtering

Document filtering can be enabled by passing a `filter` parameter in the `check` request body when starting a check. The filter takes a JSON query. The query syntax is identical to the [read operation query syntax](https://www.mongodb.com/docs/manual/tutorial/query-documents/#std-label-read-operations-query-argument). For example, running the following command makes the verifier check to only check documents within the filter `{"inFilter": {"$ne": false}}` for _all_ namespaces:

```
curl -H "Content-Type: application/json" -X POST -d '{{"filter": {"inFilter": {"$ne": false}}}}' http://127.0.0.1:27020/api/v1/check
```
If a checking is started with the above filter, the table below summarizes the verifier's behavior: 

| Source Document                                   | Destination Document                              | Verifier's Behavior                         |
|---------------------------------------------------|---------------------------------------------------|---------------------------------------------|
| `{"_id": <id>, "inFilter": true, "diff": "src"}`  | `{"_id": <id>, "inFilter": true, "diff": "dst"}`  | ❗ (Finds a document with differing content) |
| `{"_id": <id>, "inFilter": false, "diff": "src"}` | `{"_id": <id>, "inFilter": false, "diff": "dst"}` | ✅ (Skips a document)                        |
| `{"_id": <id>, "inFilter": true, "diff": "src"}`  | `{"_id": <id>, "inFilter": false, "diff": "dst"}` | ❗ (Finds a document missing on Destination) |
| `{"_id": <id>, "inFilter": false, "diff": "src"}` | `{"_id": <id>, "inFilter": true, "diff": "dst"}`  | ❗ (Finds a document missing on Source)      |

# Checking Failures

Use the [`/docMismatches`](#documents) and [`/nsMismatches`](#namespaces) APIs
to inspect currently-tracked failures. Document mismatches (missing, extra, or
differing content) appear in `/docMismatches`; namespace-level mismatches
(missing collections, index differences, spec differences, etc.) appear in
`/nsMismatches`.

# Resumability

The migration-verifier periodically persists its change stream’s resume token so that, in the event of a catastrophic failure (e.g., memory exhaustion), when restarted the verifier will receive any change events that happened while the verifier was down.

# Performance

The verifier has been observed handling test source write loads of 15,000 writes per second. Real-world performance will vary according to several factors, including network latency, cluster resources, and the verifier node’s resources.

## Change stream lag

Every time the verifier notices a change in a document, it schedules a recheck
of that document. If the changes happen faster than the verifier can schedule
rechecks, then the verifier “lags” the cluster. We measure that lag by
comparing the server-reported cluster time with the time of the most
recently-seen event.

If the lag exceeds a certain “comfortable” threshold, the verifier will warn
in the logs. High lag can cause either of these outcomes:

1. Once writes stop on the source (i.e., during the migration’s cutover),
you’ll have to wait for a longer-than-ideal time for the verifier to recheck
documents until its writes-off timestamp.

2. Sufficiently high verifier lag can exceed the server’s oplog capacity. If
this happens, verification will fail permanently, and you’ll have to restart
verification from the beginning.

### Mitigation

The following may help if you see warnings about change stream lag:

1. Scale up: Run the verifier on a more powerful host.

2. Reduce load: Disable nonessential applications during verification until cutover.

## Recheck generation size

Even if the change stream keeps up with the write load, the verifier may still recheck
the documents more slowly than writes happen on the source. If this happens, you’ll
see recheck generations grow over time.

Unlike change stream lag, this won’t actually endanger the verification. It will, though,
extend downtime during cutover because the final recheck generation will take longer than
it otherwise might.

### Mitigation

1. Scale up. (See above.)

2. Reduce load. (ditto)

3. Make the verifier compare document hashes rather than full documents. See below for details.

## Per-shard verification

If migrating shard-to-shard, you can also verify shard-to-shard to scale verification horizontally. Run 1 verifier per source shard. You can colocate all verifiers’ metadata on the same metadata cluster, but each verifier must use its own database (e.g., `verify90`, `verify1`, …). If that metadata cluster buckles under the load, consider splitting verification across multiple hosts.

# Document comparison methods

## `binary`

The default. This establishes full binary equivalence, including field order and all types.

## `ignoreFieldOrder`

Like `binary` but ignores the ordering of fields. Incurs extra overhead on the verifier host.

## `toHashedIndexKey`

Compares document hashes (and lengths) rather than full documents. This minimizes the data sent to migration-verifier, which can dramatically increase performance.

It carries a few downsides, though:

### Lost precision

This method ignores certain type changes if the underlying value remains the same. For example, if a Long changes to a Double, and the two values are identical, `toHashedIndexKey` will not notice the discrepancy.

The discrepancy _will_, though, usually be seen if the BSON types are of different lengths. For example, if a Long changes to Decimal, `toHashedIndexKey` will notice that.

If, however, _multiple_ numeric type changes happen, then `toHashedIndexKey` will only notice the discrepancy if the total document length changes. For example, if an Int changes to a Long, but elsewhere a Long changes to an Int, that will evade notice.

The above are all **highly** unlikely in real-world migrations.

### Lost reporting

Full-document verification methods allow migration-verifier to diagnose mismatches, e.g., by identifying specific changed fields. The only such detail that `toHashedIndexKey` can discern, though, is a change in document length.

Additionally, because the amount of data sent to migration-verifier doesn’t actually reflect the documents’ size, no meaningful statistics are shown concerning the collection data size. Document counts, of course, are still shown.

# Partitioning schemes

## `_id`

The default. Collections are partitioned along their `_id` index. This works for
all collections & topologies.

## `natural`

This partitions collections on the source by their record ID, which corresponds
to their location in storage. This can greatly accelerate verification, and
reduce server load, for collections with custom `_id` values.

The following caveats apply:

### MongoDB 4.2 & earlier

Source clusters running MongoDB 4.2 & earlier cannot parallelize collection
scans in natural mode. Each non-empty collection is scanned in exactly one
task. If Verifier is interrupted and has to restart, the entire collection
scan restarts from the beginning.

### Unsupported configurations

This scheme only works when connecting to a replica set (i.e., not a mongos).

### Lost checks

Under this method, Migration Verifier fetches documents from the source in
natural (i.e., on-disk) order. After fetching a batch of documents from the
source, Migration Verifier fetches those same documents by `_id` from the
destination. There is no full collection scan on the destination.
Because of this, under natural partitioning Migration Verifier cannot detect
documents that exist only on the destination _unless_ such documents change
(or are added) during verification.

As of this writing, no migration tooling from MongoDB is expected to create
such documents.

### Resumption and document deletion

Document deletions can complicate resumption of natural
scans (i.e., after a restart). When reading documents from a given record ID,
if the record ID’s referent document has been deleted, the server (prior to 7.0)
cannot resume directly from that record ID.

To compensate, in such cases Migration Verifier will read lower record IDs from
other verification tasks and try to resume from them. In effect, 2 or more
tasks’ documents are read in a single cursor. Migration Verifier will
discard any documents that aren’t actually part of the partition to be
compared.

If, however, the server runs 7.0.26+ or 8.0.14+, then this is not necessary
because these server versions expose a control that obviates this workaround.

(See [SERVER-110161](https://jira.mongodb.org/browse/SERVER-110161) for more
details.)

This also does not affect source clusters running MongoDB 4.2 or earlier
because such clusters can’t parallelize collection scans anwyay.

# Change reading methods

NB: If the verifier restarts, it **MUST** use the same change reader options
as before, or it will fail immediately.

## `changeStream`

The default. The verifier will read a change stream, which works seamlessly on sharded or unsharded clusters.

## `tailOplog`

The verifier will read the oplog continually instead of reading a change stream. This is generally faster, but it only works when connecting to a replica set (i.e., not a mongos).

# Amazon DocumentDB sources

The verifier can verify a migration **from** Amazon DocumentDB **to** MongoDB.
DocumentDB emulates MongoDB's wire protocol and reports a MongoDB version
number (5.0.0), but does not implement many of the features that version
implies, so the verifier detects it and adapts. Detection is automatic;
`--srcFlavor documentdb` forces it if that ever misfires.

DocumentDB as a *destination* or as the *metadata* cluster is not supported.

## Connecting

DocumentDB requires TLS, and a tunnelled connection needs some care:

```
--srcURI "mongodb://user:pass@host:27017/?tls=true\
&tlsCAFile=/path/to/global-bundle.pem\
&directConnection=true\
&retryWrites=false"
```

- **`directConnection=true`** — without it the driver performs replica-set
  discovery, learns the cluster's internal instance endpoints, and tries to
  connect to those directly. The verifier adds this automatically for a
  single-host URI with no `replicaSet`.
- **`retryWrites=false`** — DocumentDB does not support retryable writes.
- **`tlsAllowInvalidHostnames` does not work.** That is a `mongosh` option; the
  Go driver accepts only `tlsInsecure`. If you are tunnelling and the
  certificate name will not match, either add a hosts entry so the real
  hostname resolves locally (preferred — certificates carry no port, so a
  forwarded port is fine) or use `tlsInsecure=true`.
- The read preference must resolve to the primary; see below.

## Required setup: enable change streams

DocumentDB disables change streams by default, and a change stream opened
against a namespace that has them disabled simply yields no events. The
verifier would then miss every concurrent write and report a match it cannot
justify, so it **refuses to start** unless the namespaces it will verify have
change streams enabled.

```js
// per database
db.adminCommand({modifyChangeStreams: 1, database: "mydb", collection: "", enable: true})

// cluster-wide, which --verifyAll requires
db.adminCommand({modifyChangeStreams: 1, database: "", collection: "", enable: true})
```

`--verifyAll` requires the cluster-wide form, because a database created
mid-run would otherwise go unwatched.

## Options the verifier rejects for a DocumentDB source

Each of these fails at startup with an explanation:

| Option | Why |
|---|---|
| `--srcChangeReader tailOplog` | DocumentDB has no operations log. |
| `--docCompareMethod toHashedIndexKey` | Needs a MongoDB-internal aggregation operator. Use `binary` or `ignoreFieldOrder`. |
| `--readPreference secondary`/`secondaryPreferred`/`nearest` | See the consistency note below. |
| `--partitioningScheme natural` | DocumentDB has no `$natural` ordering. Use `_id`. |
| A DocumentDB `--metaURI` | The verifier reads its own recheck queue through a causally-consistent session, which DocumentDB cannot provide. |

## How correctness is maintained without causal consistency

Against MongoDB, every document read is pinned with
`readConcern: {afterClusterTime: …}` so it cannot observe a state older than
the change reader's start point. DocumentDB implements no causal consistency
and rejects such reads outright, so the verifier relies instead on two facts:

1. The change reader is opened before any scan read is issued.
2. Reads from a DocumentDB **primary** are read-after-write consistent.

A write therefore either committed before a scan read — in which case the
primary shows it to us — or after it, in which case its timestamp follows the
reader's start point and the change stream captures it. This is why a
secondary read preference is rejected: replica reads are only eventually
consistent, which would break the argument.

## Watch out for

### Change stream retention

DocumentDB retains change stream events for **3 hours by default** (7 days
maximum). If the verifier is interrupted for longer than the window, its resume
token expires. Resuming past that point would leave an unobserved gap, so the
verifier **aborts and requires a restart with `--clean`** rather than
continuing.

Raise `change_stream_log_retention_duration` on the cluster parameter group for
any long verification. The setting cannot be read over the MongoDB connection
(`getParameter` is unimplemented), so the verifier warns unconditionally rather
than checking.

### The change reader is much slower than DocumentDB accepts writes

In testing, DocumentDB accepted ~23,000 inserts/sec while the verifier's change
reader consumed ~800–1,400 events/sec. **No events were lost**: a 60-second
burst of 1,380,500 inserts was eventually consumed in full and every one of the
1,380,500 documents was reported. But working through it took ~20 minutes.

Under sustained writes above roughly 1–2k/sec the recheck queue will not
converge, and a reader that falls far enough behind can outrun the retention
window above. That measurement was taken over an SSH tunnel, which penalises
the reader's round-trip-bound `getMore` loop far more than it does batched
writes, so expect better in-VPC — but measure before relying on it.

### Let bulk writes finish before turning writes off

DocumentDB's multi-document writes are atomic, so a large `updateMany` that is
still running when you turn writes off commits *entirely* after the writes-off
timestamp and is excluded from verification wholesale. On MongoDB the same
write produces per-document oplog entries, so the portion before the cutoff
would still be caught.

Wait for bulk operations to complete before turning writes off. (Writing after
writes-off is out of contract either way; DocumentDB just makes the
consequence sharper.)

### Index changes during a run are invisible

DocumentDB emits only `insert`, `update`, `delete`, and `drop` change events.
Creating a collection, and **all index DDL**, produce no events at all. So an
index created or dropped mid-run is never noticed — indexes are compared once,
during generation 0. Dropping a *collection* does emit `drop`, which aborts the
run.

### Metadata is normalised, not compared verbatim

DocumentDB and MongoDB describe the same collection differently. The verifier
strips the differences that describe the engine rather than the data, so that
mismatches reflect real divergence:

- Collection options: `storageEngine` and `autoIndexId` are dropped, and
  `capped: false` is treated as equivalent to the field being absent.
- Index specifications: `ns` and `2dsphereIndexVersion` are dropped, and `v` is
  normalised (DocumentDB reports `v: 4`, MongoDB `v: 2`).

Everything that defines an index — key, `unique`, `sparse`,
`partialFilterExpression`, `expireAfterSeconds` — is compared normally.

### Not applicable on DocumentDB

Capped collections and views cannot be created on DocumentDB, so the verifier's
handling of both is unreachable from a DocumentDB source.

## Verification status

The DocumentDB support was validated against a live DocumentDB 5.0.0 cluster:
250,000 documents across 5 collections, every BSON type and index type
DocumentDB supports, deliberate mismatch injection, a forced primary failover
(no events lost), resume-token expiry, and a 1.38-million-event write burst.

`architecture/documentdb_open_questions.md` records every assumption the
support rests on, how it was confirmed, and what remains unverified.
`architecture/documentdb_probe.js` re-checks most of them against a cluster in
one run.

# Known Issues

- If memory usage rises after generation 0, try reducing `recheckMaxSizeMB`. This will shrink the queries that the verifier sends, which in turn should reduce the server’s memory usage. (The number of actual queries sent will rise, of course.)

## Time-Series Collections

Because the verifier compares documents by `_id`, it cannot compare logical time-series measurements (i.e., the data that users actually insert). Instead it compares the server’s internal time-series “buckets”. Unfortunately, this makes mismatch details essentially useless with time-series since they will be details about time-series buckets, which users generally don’t see.

It also requires that migrations replicate the raw buckets rather than the logical measurements. This is because a logical migration would cause `_id` mismatches between source & destination buckets. A user application wouldn’t care (since it never sees the buckets’ `_id`s), but verification does.

NB: Given bucket documents’ size, hashed document comparison can be especially useful with time-series.

# Limitations

- The verifier does not verify DDL changes. By default, such changes will crash the verifier. (See the `--ddlHandling` option above for alternative behavior.)

- Amazon DocumentDB sources carry additional limitations; see [Amazon DocumentDB sources](#amazon-documentdb-sources).

- The verifier cannot verify time-series collections under namespace filtering.
