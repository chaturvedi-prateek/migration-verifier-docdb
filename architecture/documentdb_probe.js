// DocumentDB assumption probe.
//
// Runnable companion to documentdb_open_questions.md. Each check answers one
// numbered question there and prints PASS / FAIL / INFO. Run it against a real
// DocumentDB cluster and record the results in that file.
//
//   mongosh "mongodb://USER:PASS@localhost:27017/?tls=true\
//     &tlsCAFile=/path/global-bundle.pem&tlsAllowInvalidHostnames=true\
//     &directConnection=true" --quiet --file architecture/documentdb_probe.js
//
// Set ALLOW_WRITES to false to run only the read-only checks. When true, the
// script creates and then DROPS the database named in PROBE_DB.

const ALLOW_WRITES = true;
const PROBE_DB = "__mv_probe";

let pass = 0, fail = 0, info = 0;

// mongosh does not define the legacy shell's tojson(), and JSON.stringify
// mangles BSON types such as Timestamp, so prefer EJSON when it is available.
function fmt(v) {
  try { return EJSON.stringify(v); }
  catch (e) { try { return JSON.stringify(v); } catch (e2) { return String(v); } }
}

function ok(q, msg)   { print(`PASS  ${q}  ${msg}`); pass++; }
function bad(q, msg)  { print(`FAIL  ${q}  ${msg}`); fail++; }
function note(q, msg) { print(`INFO  ${q}  ${msg}`); info++; }

function attempt(q, label, fn) {
  try { fn(); }
  catch (e) { bad(q, `${label} threw: ${e.message || e}`); }
}

print("=== DocumentDB assumption probe ===\n");

// ---------- Q9 / Q21 / Q17: identity and clocks ----------
attempt("Q9 ", "hello", () => {
  const h = db.adminCommand({ hello: 1 });
  const hasMe = h.me !== undefined;
  const hasCT = h.$clusterTime !== undefined;

  note("Q9 ", `hello: me=${h.me} setName=${h.setName} msg=${h.msg}`);

  if (hasMe && !hasCT) {
    ok("Q9 ", "advertises a replica set and omits $clusterTime -> detected as DocumentDB");
  } else if (hasMe && hasCT) {
    bad("Q9 ", "gossips $clusterTime -> would be detected as MongoDB. Use --srcFlavor documentdb");
  } else {
    bad("Q9 ", "no 'me' field -> would be detected as a standalone MongoDB");
  }

  if (h.operationTime !== undefined) {
    ok("Q21", `hello returns operationTime (${fmt(h.operationTime)}) -> writes-off fence works`);
  } else {
    bad("Q21", "hello has NO operationTime -> WritesOff will fail; find another command for getDocumentDBServerTime");
  }
});

attempt("Q17", "buildInfo", () => {
  const b = db.adminCommand({ buildInfo: 1 });
  if (Array.isArray(b.versionArray)) {
    ok("Q17", `versionArray=${fmt(b.versionArray)} version=${b.version}`);
  } else {
    bad("Q17", `no versionArray; version=${b.version}`);
  }
});

// ---------- Q7: operationTime from other commands ----------
attempt("Q7 ", "operationTime survey", () => {
  const cmds = [{ ping: 1 }, { dbStats: 1 }, { listDatabases: 1 }];
  const found = [];
  for (const c of cmds) {
    try {
      const r = db.adminCommand(c);
      if (r.operationTime !== undefined) found.push(Object.keys(c)[0]);
    } catch (e) { /* command may be unsupported; not fatal */ }
  }
  if (found.length) ok("Q7 ", `commands returning operationTime: ${found.join(", ")}`);
  else note("Q7 ", "none of ping/dbStats/listDatabases returned operationTime");
});

// ---------- Q5: change streams enabled ----------
attempt("Q5 ", "$listChangeStreams", () => {
  const r = db.getSiblingDB("admin").runCommand({
    aggregate: 1, pipeline: [{ $listChangeStreams: 1 }], cursor: {},
  });
  if (r.ok !== 1) { bad("Q5 ", `returned ok=${r.ok}: ${r.errmsg}`); return; }

  const scopes = r.cursor.firstBatch;
  ok("Q5 ", `works; ${scopes.length} enabled scope(s): ${fmt(scopes)}`);

  if (scopes.some(s => s.database === "" && s.collection === "")) {
    ok("Q5 ", "cluster-wide wildcard present -> verifyAll runs will pass Gate A");
  } else {
    note("Q5 ", 'no cluster-wide wildcard; verifyAll needs modifyChangeStreams with database:"" collection:""');
  }
});

// ---------- Q20: config.collections ----------
attempt("Q20", "config.collections", () => {
  const r = db.getSiblingDB("config").runCommand({
    find: "collections", filter: { _id: "nosuch.ns" }, limit: 1,
  });
  if (r.ok === 1) ok("Q20", "readable (empty result) -> getShardKeyFields degrades to 'not sharded'");
  else bad("Q20", `errored ok=${r.ok}: ${r.errmsg} -> partitioning would fail per collection`);
});

// ---------- Q6: read concern ----------
attempt("Q6 ", "majority read concern", () => {
  const r = db.getSiblingDB(PROBE_DB).runCommand({
    find: "probe", limit: 1, readConcern: { level: "majority" },
  });
  if (r.ok === 1) ok("Q6 ", "majority read concern accepted");
  else bad("Q6 ", `rejected: ${r.errmsg} -> skip client-level majority for DocumentDB`);
});

if (!ALLOW_WRITES) {
  print(`\n=== ${pass} pass, ${fail} fail, ${info} info (write checks skipped) ===`);
  quit(fail > 0 ? 1 : 0);
}

// ---------- write-dependent checks ----------
const pdb = db.getSiblingDB(PROBE_DB);
pdb.dropDatabase();

const N = 5000;
attempt("--", "seed", () => {
  const bulk = [];
  for (let i = 0; i < N; i++) bulk.push({ _id: i, payload: "x".repeat(200) });
  pdb.docs.insertMany(bulk);
  note("--", `seeded ${N} docs into ${PROBE_DB}.docs`);
});

// ---------- Q1: primary read-after-write ----------
attempt("Q1 ", "read-after-write", () => {
  let misses = 0;
  for (let i = 0; i < 300; i++) {
    const id = `raw-${i}`;
    pdb.raw.insertOne({ _id: id, v: i });
    if (pdb.raw.findOne({ _id: id }) === null) misses++;
  }
  if (misses === 0) ok("Q1 ", "300/300 immediate reads observed their own write");
  else bad("Q1 ", `${misses}/300 reads MISSED their own write -> the consistency design does not hold`);
});

// ---------- Q10: covered index scan ----------
//
// DocumentDB's explain output does not follow MongoDB's
// executionStats.totalDocsExamined shape, so we probe for several field names
// and dump the raw document when none match. Read the dump, then decide.
attempt("Q10", "index-walk explain", () => {
  const e = pdb.docs.find({ _id: { $gt: 100 } }, { _id: 1 })
    .sort({ _id: 1 }).hint({ _id: 1 }).skip(999).limit(1)
    .explain("executionStats");

  const st = e.executionStats || {};

  // MongoDB reports totalDocsExamined. DocumentDB reports no such counter and
  // instead names the stage: IXONLYSCAN is its index-only (covered) scan.
  const docs = st.totalDocsExamined ?? st.docsExamined;

  if (docs !== undefined) {
    note("Q10", `docsExamined=${docs} keysExamined=${st.totalKeysExamined ?? st.keysExamined}`);
    if (docs === 0) ok("Q10", "covered scan: zero documents fetched");
    else bad("Q10", `fetched ${docs} docs -> the index walk is costlier than designed`);
    return;
  }

  const planText = fmt(e.queryPlanner ? e.queryPlanner.winningPlan : e);

  if (planText.indexOf("IXONLYSCAN") !== -1) {
    ok("Q10", `index-only scan (IXONLYSCAN over ${e.queryPlanner.winningPlan.inputStage.indexName}) -> no documents fetched`);
    note("Q10", `plan: ${planText}`);
  } else if (planText.indexOf("COLLSCAN") !== -1) {
    bad("Q10", `collection scan -> the index walk would be prohibitively costly. plan: ${planText}`);
  } else {
    note("Q10", `unrecognised plan shape; inspect manually: ${planText}`);
    print(fmt(e));
  }
});

// ---------- Q11: $collStats ----------
attempt("Q11", "$collStats", () => {
  const r = pdb.runCommand({
    aggregate: "docs",
    pipeline: [
      { $collStats: { storageStats: { scale: 1 } } },
      { $group: { _id: "$ns", count: { $sum: "$storageStats.count" },
                  size: { $sum: "$storageStats.size" }, capped: { $first: "$capped" } } },
    ],
    cursor: {},
  });
  if (r.ok !== 1) { bad("Q11", `failed: ${r.errmsg} -> cannot size partitions`); return; }
  const d = r.cursor.firstBatch[0];
  if (d && d.count > 0 && d.size > 0) ok("Q11", `ns=${d._id} count=${d.count} size=${d.size} capped=${d.capped}`);
  else bad("Q11", `returned unusable stats: ${fmt(d)}`);
});

// ---------- Q12: listCollections info.uuid ----------
attempt("Q12", "listCollections", () => {
  const r = pdb.runCommand({ listCollections: 1, filter: { name: "docs" } });
  const spec = r.cursor.firstBatch[0];
  if (spec && spec.info && spec.info.uuid !== undefined) ok("Q12", "info.uuid IS reported");
  else note("Q12", `no info.uuid (expected). spec=${fmt(spec)}`);
});

// ---------- Q13: capped collections ----------
attempt("Q13", "capped collection", () => {
  try {
    pdb.createCollection("capped", { capped: true, size: 100000 });
    note("Q13", "capped collections ARE supported -> _id-order comparison applies");
  } catch (e) {
    ok("Q13", `capped collections rejected (${e.codeName || e.message}) -> case cannot arise`);
  }
});

// ---------- Q2 / Q3 / Q15: change stream shape ----------
attempt("Q2 ", "change stream", () => {
  let cur;
  try {
    cur = pdb.docs.watch();
  } catch (e) {
    bad("Q2 ", `cannot open change stream (enable it first?): ${e.message}`);
    return;
  }

  pdb.docs.insertOne({ _id: "cs-probe", v: 1 });
  pdb.docs.updateOne({ _id: "cs-probe" }, { $set: { v: 2 } });

  let evt = null;
  for (let i = 0; i < 40 && evt === null; i++) { if (cur.hasNext()) evt = cur.next(); }

  if (!evt) { bad("Q2 ", "no change event arrived within the polling window"); return; }

  note("Q2 ", `event keys: ${Object.keys(evt).join(", ")}`);
  if (evt.clusterTime !== undefined) ok("Q2 ", `events carry clusterTime (${fmt(evt.clusterTime)})`);
  else bad("Q2 ", "events have NO clusterTime -> recheck ordering and the drain lose their timestamp");

  const token = evt._id;
  note("Q3 ", `resume token: ${fmt(token)}`);
  if (token && typeof token._data === "string") {
    ok("Q3 ", `_data is a ${token._data.length}-char string (opaque; not decoded as KeyString)`);
  }

  // Q15: startAfter should be rejected.
  try {
    pdb.docs.watch([], { startAfter: token }).hasNext();
    note("Q15", "startAfter was ACCEPTED -> the flavor gate is merely unnecessary, not wrong");
  } catch (e) {
    ok("Q15", `startAfter rejected (${e.codeName || e.message}) -> gate is required`);
  }
});

pdb.dropDatabase();
print(`\ndropped ${PROBE_DB}`);
print(`\n=== ${pass} pass, ${fail} fail, ${info} info ===`);
print("Record results in architecture/documentdb_open_questions.md (mark items CLUSTER + version + date).");
