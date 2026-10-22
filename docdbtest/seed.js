// Deterministic dataset seeder for the DocumentDB verification suite.
//
// Run against BOTH source and destination to produce identical data, so a
// clean verification is the expected result without needing a migration tool.
//
//   mongosh "<uri>" --eval 'var DB="mvsuite", DOCS=50000, COLLS=1' --file seed.js
//
// Covers every BSON type DocumentDB supports and every index type/property it
// supports. Deliberately omits types DocumentDB rejects (JavaScript, Symbol,
// Undefined) and constructs it rejects (capped collections, views).

const DBNAME = typeof DB === "undefined" ? "mvsuite" : DB;
const NDOCS = typeof DOCS === "undefined" ? 10000 : DOCS;
const NCOLLS = typeof COLLS === "undefined" ? 1 : COLLS;
const BATCH = 1000;

const d = db.getSiblingDB(DBNAME);
d.dropDatabase();

function hex24(i) { return i.toString(16).padStart(24, "0"); }
function rnd(seed) { return ((seed * 1103515245 + 12345) & 0x7fffffff) / 0x7fffffff; }

for (let c = 0; c < NCOLLS; c++) {
  const name = NCOLLS === 1 ? "alltypes" : "alltypes" + c;
  const coll = d[name];
  let batch = [];

  for (let i = 0; i < NDOCS; i++) {
    batch.push({
      _id: i,
      dbl: 3.14159 * (i + 1),
      str: "string-value-" + i + "-" + "pad".repeat(20),
      obj: { nested: { depth: 2, label: "n" + i }, flag: i % 2 === 0 },
      arr: [i, i + 1, i + 2, "s" + i],
      arrObj: [{ k: "a", v: i }, { k: "b", v: i * 2 }],
      bin: BinData(0, "SGVsbG8gRG9jdW1lbnREQg=="),
      binUuid: UUID("aaaaaaaa-bbbb-cccc-dddd-" + i.toString(16).padStart(12, "0")),
      oid: ObjectId(hex24(i)),
      bool: i % 3 === 0,
      date: new Date(1600000000000 + i * 1000),
      nul: null,
      rgx: /^prefix-[0-9]+$/i,
      i32: NumberInt(i % 2147483647),
      i64: NumberLong("9007199254740" + (i % 1000).toString().padStart(3, "0")),
      ts: Timestamp({ t: 1600000000 + i, i: 1 }),
      dec: NumberDecimal((i + 0.12345).toFixed(5)),
      minK: MinKey(),
      maxK: MaxKey(),
      uniq: i,
      sparseF: i % 5 === 0 ? i : undefined,
      partialF: i,
      status: i % 4 === 0 ? "active" : "inactive",
      // Fixed far-future date: Date.now() would differ between the two
      // clusters and mismatch every document, and a past date would let the
      // TTL index delete rows mid-run.
      ttlAt: new Date(4000000000000 + i),
      loc: { type: "Point", coordinates: [(rnd(i) * 360) - 180, (rnd(i + 7) * 180) - 90] },
      tags: ["t" + (i % 50), "t" + (i % 7)],
    });

    if (batch.length === BATCH) { coll.insertMany(batch, { ordered: false }); batch = []; }
  }
  if (batch.length) coll.insertMany(batch, { ordered: false });

  const indexes = [
    ["single",     () => coll.createIndex({ str: 1 }, { name: "ix_single" })],
    ["compound",   () => coll.createIndex({ status: 1, i32: -1 }, { name: "ix_compound" })],
    ["multikey",   () => coll.createIndex({ tags: 1 }, { name: "ix_multikey" })],
    ["nested",     () => coll.createIndex({ "obj.nested.label": 1 }, { name: "ix_nested" })],
    ["unique",     () => coll.createIndex({ uniq: 1 }, { name: "ix_unique", unique: true })],
    ["sparse",     () => coll.createIndex({ sparseF: 1 }, { name: "ix_sparse", sparse: true })],
    ["partial",    () => coll.createIndex({ partialF: 1 }, { name: "ix_partial", partialFilterExpression: { status: "active" } })],
    ["ttl",        () => coll.createIndex({ ttlAt: 1 }, { name: "ix_ttl", expireAfterSeconds: 2592000 })],
    ["2dsphere",   () => coll.createIndex({ loc: "2dsphere" }, { name: "ix_geo" })],
    ["descending", () => coll.createIndex({ date: -1 }, { name: "ix_desc" })],
  ];
  const skipped = [];
  for (const [label, fn] of indexes) {
    try { fn(); } catch (e) { skipped.push(label + "(" + (e.codeName || e.message).substring(0, 40) + ")"); }
  }
  if (skipped.length) print("SEED WARN indexes skipped on " + name + ": " + skipped.join(" "));
}

// _ids spanning many BSON types, to exercise type bracketing in the _id walk.
d.mixedids.insertMany([
  { _id: MinKey(), kind: "minkey" },
  { _id: NumberInt(1), kind: "int32" },
  { _id: NumberLong("2"), kind: "int64" },
  { _id: 3.5, kind: "double" },
  { _id: NumberDecimal("4.25"), kind: "decimal" },
  { _id: "str-id", kind: "string" },
  { _id: ObjectId(hex24(999999)), kind: "objectid" },
  { _id: new Date(1600000000000), kind: "date" },
  { _id: true, kind: "bool" },
  { _id: BinData(0, "AQIDBA=="), kind: "binary" },
  { _id: { a: 1, b: 2 }, kind: "object" },
  { _id: MaxKey(), kind: "maxkey" },
], { ordered: false });

let total = 0;
d.getCollectionNames().forEach(n => { total += d[n].countDocuments(); });
print("SEED OK db=" + DBNAME + " collections=" + d.getCollectionNames().length + " docs=" + total);
