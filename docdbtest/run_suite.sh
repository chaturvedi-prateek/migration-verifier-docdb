#!/usr/bin/env bash
#
# DocumentDB -> MongoDB/Atlas verification suite. Fully self-contained.
#
# It obtains its own tooling, fixes up the source URI, enables the change
# streams it needs, runs every scenario, reviews the logs, and tears down
# everything it created.
#
#   export SRC_URI='mongodb://user:pass@docdb-endpoint:27017/'
#   export DST_URI='mongodb+srv://user:pass@atlas.mongodb.net/'
#   ./docdbtest/run_suite.sh
#
# Optional:
#   META_URI      metadata cluster (default DST_URI; must not be DocumentDB)
#   MV_BIN        verifier binary (default: repo binary, else built from source)
#   OUT_DIR       log directory (default ./docdbtest-logs-<timestamp>)
#   DOCS          docs per collection for baseline (default 50000)
#   SCALE_COLLS   scale collections (default 5)
#   SCALE_DOCS    docs per scale collection (default 50000)
#   TESTS         subset of test names (default all)
#   KEEP_DATA=1   skip teardown, for debugging a failure
#   AUTO_INSTALL=1  allow installing mongosh via the system package manager
#
set -uo pipefail

SUITE_DB="mvsuite"
SCALE_DB="mvscale"
PORT="${PORT:-27020}"
DOCS="${DOCS:-50000}"
SCALE_COLLS="${SCALE_COLLS:-5}"
SCALE_DOCS="${SCALE_DOCS:-50000}"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OUT_DIR="${OUT_DIR:-$PWD/docdbtest-logs-$(date +%Y%m%d-%H%M%S)}"
CA_BUNDLE="${CA_BUNDLE:-$HERE/global-bundle.pem}"

PASS=0; FAIL=0; SKIP=0
declare -a RESULTS=()
declare -a ENABLED_SCOPES=()   # change stream scopes we turned on, to turn off later
CLUSTERWIDE_ENABLED=0

die () { echo "FATAL: $*" >&2; exit 1; }
log () { echo "[$(date +%H:%M:%S)] $*"; }
step () { log "  .. $*"; }

record () {
  RESULTS+=("$1|$2|$3")
  case "$1" in
    PASS) PASS=$((PASS+1)); echo "  PASS  $2  $3" ;;
    FAIL) FAIL=$((FAIL+1)); echo "  FAIL  $2  $3" ;;
    SKIP) SKIP=$((SKIP+1)); echo "  SKIP  $2  $3" ;;
  esac
}

# Never echo a URI; they carry credentials.
msh () { local uri="$1"; shift; mongosh "$uri" --quiet "$@" 2>&1; }

# =========================================================== phase 0: prep ===

[ -n "${SRC_URI:-}" ] || die "SRC_URI is required"
[ -n "${DST_URI:-}" ] || die "DST_URI is required"
META_URI="${META_URI:-$DST_URI}"

ensure_mongosh () {
  command -v mongosh >/dev/null && return 0
  step "mongosh not found"
  if [ "${AUTO_INSTALL:-0}" != "1" ]; then
    die "mongosh is required. Install it, or re-run with AUTO_INSTALL=1 to let this script install it.
     Amazon Linux / RHEL:  sudo dnf install -y mongodb-mongosh
     Debian / Ubuntu:      sudo apt-get install -y mongodb-mongosh"
  fi
  step "installing mongosh (AUTO_INSTALL=1)"
  if command -v dnf >/dev/null; then
    sudo dnf install -y mongodb-mongosh >/dev/null 2>&1 || \
    { printf '[mongodb-org-7.0]\nname=MongoDB\nbaseurl=https://repo.mongodb.org/yum/amazon/2023/mongodb-org/7.0/x86_64/\ngpgcheck=0\nenabled=1\n' \
        | sudo tee /etc/yum.repos.d/mongodb-org-7.0.repo >/dev/null
      sudo dnf install -y mongodb-mongosh >/dev/null 2>&1; }
  elif command -v apt-get >/dev/null; then
    sudo apt-get update -qq && sudo apt-get install -y mongodb-mongosh >/dev/null 2>&1
  fi
  command -v mongosh >/dev/null || die "mongosh install failed; install it manually"
}

ensure_ca_bundle () {
  [ -s "$CA_BUNDLE" ] && { step "CA bundle present"; return 0; }
  step "downloading DocumentDB CA bundle"
  local url="https://truststore.pki.rds.amazonaws.com/global/global-bundle.pem"
  if command -v curl >/dev/null; then curl -sS -o "$CA_BUNDLE" "$url"
  elif command -v wget >/dev/null; then wget -q -O "$CA_BUNDLE" "$url"
  else die "need curl or wget to fetch the CA bundle"; fi
  [ -s "$CA_BUNDLE" ] || die "CA bundle download failed"
}

# Append a query parameter only if absent, handling the ?/& split.
uri_param () { # uri_param <uri> <key=value>
  local uri="$1" kv="$2" key="${2%%=*}"
  case "$uri" in *"$key="*) echo "$uri"; return ;; esac
  case "$uri" in
    *\?*) echo "${uri}&${kv}" ;;
    */)   echo "${uri}?${kv}" ;;
    *)    echo "${uri}/?${kv}" ;;
  esac
}

# DocumentDB needs TLS with the RDS CA, a direct connection (otherwise the
# driver chases internal instance endpoints), and retryable writes off.
normalize_src_uri () {
  SRC_URI=$(uri_param "$SRC_URI" "tls=true")
  SRC_URI=$(uri_param "$SRC_URI" "tlsCAFile=$CA_BUNDLE")
  SRC_URI=$(uri_param "$SRC_URI" "directConnection=true")
  SRC_URI=$(uri_param "$SRC_URI" "retryWrites=false")
  # mongosh understands tlsAllowInvalidHostnames; the Go driver only knows
  # tlsInsecure. Keep separate URIs so each gets what it can use.
  SRC_URI_SH=$(uri_param "$SRC_URI" "tlsAllowInvalidHostnames=true")
}

resolve_binary () {
  if [ -n "${MV_BIN:-}" ]; then :
  elif [ -x "$PWD/migration_verifier" ]; then MV_BIN="$PWD/migration_verifier"
  elif [ -x "$HERE/../migration_verifier" ]; then MV_BIN="$HERE/../migration_verifier"
  else
    step "no binary found; building from source"
    command -v go >/dev/null || die "no verifier binary and no Go toolchain to build one"
    (cd "$HERE/.." && go build -o "$HERE/../migration_verifier" ./main) || die "build failed"
    MV_BIN="$HERE/../migration_verifier"
  fi
  [ -x "$MV_BIN" ] || die "verifier binary not executable: $MV_BIN"
}

ensure_mongosh
ensure_ca_bundle
normalize_src_uri
resolve_binary
mkdir -p "$OUT_DIR"

log "verifier : $MV_BIN"
log "logs     : $OUT_DIR"
log "shape    : baseline ${DOCS} docs; scale ${SCALE_COLLS}x${SCALE_DOCS}"

step "checking connectivity"
msh "$SRC_URI_SH" --eval 'print("src ok " + db.adminCommand({buildInfo:1}).version)' | tail -1 | grep -q "src ok" \
  || die "cannot reach the source cluster (check SRC_URI, security groups, CA bundle)"
msh "$DST_URI" --eval 'print("dst ok " + db.adminCommand({buildInfo:1}).version)' | tail -1 | grep -q "dst ok" \
  || die "cannot reach the destination cluster (check DST_URI and Atlas IP access list)"

{
  echo "suite started: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "binary: $MV_BIN"
  "$MV_BIN" --version 2>&1 | head -2
  echo "source:"
  msh "$SRC_URI_SH" --eval 'const h=db.adminCommand({hello:1}); print("  me="+h.me+" setName="+h.setName+" clusterTime="+(h.$clusterTime!==undefined)+" operationTime="+(h.operationTime!==undefined)+" version="+db.adminCommand({buildInfo:1}).version)'
  echo "destination:"
  msh "$DST_URI" --eval 'const h=db.adminCommand({hello:1}); print("  setName="+h.setName+" clusterTime="+(h.$clusterTime!==undefined)+" version="+db.adminCommand({buildInfo:1}).version)'
} > "$OUT_DIR/00-environment.log" 2>&1
sed 's/^/    /' "$OUT_DIR/00-environment.log" | tail -5

# ======================================================= change stream setup ===

enable_change_streams () {
  local dbn="$1" label="${2:-$1}"
  local out
  out=$(msh "$SRC_URI_SH" --eval "printjson(db.adminCommand({modifyChangeStreams:1, database:'$dbn', collection:'', enable:true}))")
  if echo "$out" | grep -q "ok: 1\|\"ok\": 1"; then
    ENABLED_SCOPES+=("$dbn")
    step "change streams enabled: ${label}"
    return 0
  fi
  step "could not enable change streams for ${label}"
  return 1
}

step "enabling change streams (will be disabled during teardown)"
enable_change_streams "$SUITE_DB"
enable_change_streams "$SCALE_DB"
# Cluster-wide is what --verifyAll requires; if the user lacks the privilege we
# simply skip that one test rather than failing the suite.
if enable_change_streams "" "cluster-wide"; then CLUSTERWIDE_ENABLED=1; fi

# ============================================================== teardown ===

teardown () {
  pkill -f "$(basename "$MV_BIN") --srcURI" 2>/dev/null
  sleep 1
  [ "${KEEP_DATA:-0}" = "1" ] && { log "KEEP_DATA=1 -- leaving test data and change streams in place"; return; }
  log "tearing down"
  for scope in "${ENABLED_SCOPES[@]:-}"; do
    [ -z "$scope" ] && continue
    msh "$SRC_URI_SH" --eval "db.adminCommand({modifyChangeStreams:1, database:'$scope', collection:'', enable:false})" >/dev/null 2>&1
  done
  if [ "$CLUSTERWIDE_ENABLED" = "1" ]; then
    msh "$SRC_URI_SH" --eval "db.adminCommand({modifyChangeStreams:1, database:'', collection:'', enable:false})" >/dev/null 2>&1
  fi
  step "change streams disabled"
  for uri in "$SRC_URI_SH" "$DST_URI"; do
    msh "$uri" --eval "['$SUITE_DB','$SCALE_DB'].forEach(d=>db.getSiblingDB(d).dropDatabase());" >/dev/null 2>&1
  done
  msh "$META_URI" --eval "db.getSiblingDB('__mdb_internal_migration_verifier').dropDatabase()" >/dev/null 2>&1
  step "test databases dropped"
}
trap teardown EXIT

# ================================================================ helpers ===

mv_run () { local logfile="$1"; shift
  "$MV_BIN" --srcURI "$SRC_URI" --dstURI "$DST_URI" --metaURI "$META_URI" \
    --serverPort "$PORT" "$@" > "$logfile" 2>&1 &
}
mv_stop () { pkill -f "$(basename "$MV_BIN") --srcURI" 2>/dev/null; sleep 2; }

wait_for () { local f="$1" pat="$2" secs="$3" i=0
  while [ "$i" -lt "$secs" ]; do
    grep -q "$pat" "$f" 2>/dev/null && return 0
    sleep 2; i=$((i+2))
  done
  return 1
}

gen0_block () { local f="$1" n
  n=$(grep -n "End of Generation #0" "$f" 2>/dev/null | head -1 | cut -d: -f1)
  [ -n "$n" ] && sed -n "${n},$((n+14))p" "$f"
}

seed_both () { # seed_both <db> <docs> <colls>
  msh "$SRC_URI_SH" --eval "var DB='$1', DOCS=$2, COLLS=$3" --file "$HERE/seed.js" | tail -1
  msh "$DST_URI"    --eval "var DB='$1', DOCS=$2, COLLS=$3" --file "$HERE/seed.js" | tail -1
}

want () { [ -z "${TESTS:-}" ] && return 0; [[ " $TESTS " == *" $1 "* ]]; }

# ================================================================== tests ===

t_gates () {
  local log="$OUT_DIR/01-gates.log"; : > "$log"
  local ok=0 total=0
  check_gate () { # check_gate <name> <expected-text> <flags...>
    local name="$1" expect="$2"; shift 2
    total=$((total+1)); echo "### $name" >> "$log"
    timeout 90 "$MV_BIN" --srcURI "$SRC_URI" --dstURI "$DST_URI" --metaURI "$META_URI" \
      --srcNamespace "$SUITE_DB.alltypes" --dstNamespace "$SUITE_DB.alltypes" \
      --clean --start --serverPort "$PORT" "$@" >> "$log" 2>&1
    grep -qi "$expect" "$log" && ok=$((ok+1)) || echo "### $name DID NOT REJECT" >> "$log"
  }
  check_gate tailOplog        "operations log"                 --srcChangeReader tailOplog
  check_gate toHashedIndexKey "internal aggregation operator"  --docCompareMethod toHashedIndexKey
  check_gate secondaryRead    "eventually consistent"          --readPreference secondary
  total=$((total+1)); echo "### docdbMeta" >> "$log"
  timeout 90 "$MV_BIN" --srcURI "$SRC_URI" --dstURI "$DST_URI" --metaURI "$SRC_URI" \
    --srcNamespace "$SUITE_DB.alltypes" --dstNamespace "$SUITE_DB.alltypes" \
    --clean --start --serverPort "$PORT" >> "$log" 2>&1
  grep -qi "metadata cluster may not be DocumentDB" "$log" && ok=$((ok+1))
  [ "$ok" -eq "$total" ] && record PASS gates "$ok/$total rejections fired" \
                         || record FAIL gates "only $ok/$total rejections fired"
}

t_baseline () {
  local log="$OUT_DIR/02-baseline.log"
  step "seeding $DOCS docs on both clusters"
  seed_both "$SUITE_DB" "$DOCS" 1
  mv_run "$log" --srcNamespace "$SUITE_DB.alltypes" --srcNamespace "$SUITE_DB.mixedids" \
                --dstNamespace "$SUITE_DB.alltypes" --dstNamespace "$SUITE_DB.mixedids" \
                --clean --start --partitionSizeMB 5 --logLevel info
  if wait_for "$log" "End of Generation #0" 900; then
    gen0_block "$log" > "$OUT_DIR/02-baseline-gen0.txt"
    if grep -q "No mismatches found" "$OUT_DIR/02-baseline-gen0.txt"; then
      record PASS baseline "$(grep -oE 'Source documents compared: [0-9,]+' "$OUT_DIR/02-baseline-gen0.txt" | head -1)"
    else
      record FAIL baseline "mismatches reported on identical data"
    fi
  else
    record FAIL baseline "generation 0 did not complete within 900s"
  fi
  mv_stop
}

t_mismatch () {
  local log="$OUT_DIR/03-mismatch.log"
  step "injecting 6 known differences on the destination"
  msh "$DST_URI" --eval "
    const d=db.getSiblingDB('$SUITE_DB');
    d.alltypes.updateOne({_id:100},{\$set:{str:'TAMPERED'}});
    d.alltypes.deleteOne({_id:200});
    d.alltypes.insertOne({_id:999999999,str:'EXTRA'});
    try{d.alltypes.dropIndex('ix_desc');}catch(e){}
    try{d.alltypes.dropIndex('ix_ttl');}catch(e){}
    d.alltypes.createIndex({ttlAt:1},{name:'ix_ttl',expireAfterSeconds:99999});
    d.mixedids.updateOne({_id:'str-id'},{\$set:{kind:'TAMPERED'}});
    print('injected');" | tail -1
  mv_run "$log" --srcNamespace "$SUITE_DB.alltypes" --srcNamespace "$SUITE_DB.mixedids" \
                --dstNamespace "$SUITE_DB.alltypes" --dstNamespace "$SUITE_DB.mixedids" \
                --clean --start --partitionSizeMB 5 --logLevel info
  wait_for "$log" "End of Generation #0" 900
  sleep 15
  local found=0
  for pat in '"100"' '"200"' '"999999999"' 'ix_desc' 'ix_ttl' 'str-id'; do
    grep -q "$pat" "$log" && found=$((found+1))
  done
  [ "$found" -ge 6 ] && record PASS mismatch "all 6 injected differences detected" \
                     || record FAIL mismatch "only $found/6 detected"
  mv_stop
  step "restoring destination to match source"
  msh "$DST_URI" --eval "var DB='$SUITE_DB', DOCS=$DOCS, COLLS=1" --file "$HERE/seed.js" | tail -1
}

t_crud () {
  local log="$OUT_DIR/04-crud.log"
  local mk="const c=db.getSiblingDB('$SUITE_DB').crud; c.drop(); const b=[]; for(let i=0;i<100;i++) b.push({_id:i,v:'orig'}); c.insertMany(b); print('seeded');"
  msh "$SRC_URI_SH" --eval "$mk" | tail -1
  msh "$DST_URI"    --eval "$mk" | tail -1
  mv_run "$log" --srcNamespace "$SUITE_DB.crud" --dstNamespace "$SUITE_DB.crud" \
                --clean --start --logLevel info
  wait_for "$log" "End of Generation #0" 300
  msh "$SRC_URI_SH" --eval "
    const c=db.getSiblingDB('$SUITE_DB').crud;
    c.updateOne({_id:1},{\$set:{v:'UPDATED'}});
    c.replaceOne({_id:2},{v:'REPLACED'});
    c.deleteOne({_id:3});
    c.insertOne({_id:500,v:'NEW'});
    print('mutated');" | tail -1
  sleep 40
  local ev n=0
  ev=$(grep -E "^\| Source" "$log" | tail -1)
  echo "$ev" > "$OUT_DIR/04-crud-events.txt"
  for col in 3 4 5 6; do
    local v; v=$(echo "$ev" | awk -F'|' -v c=$col '{gsub(/[ ,]/,"",$c); print $c}')
    [ -n "$v" ] && [ "$v" -gt 0 ] 2>/dev/null && n=$((n+1))
  done
  [ "$n" -eq 4 ] && record PASS crud "insert/update/replace/delete all observed" \
                 || record FAIL crud "only $n/4 change types observed"
  mv_stop
}

t_writesoff () {
  local log="$OUT_DIR/05-writesoff.log"
  mv_run "$log" --srcNamespace "$SUITE_DB.alltypes" --dstNamespace "$SUITE_DB.alltypes" \
                --clean --start --partitionSizeMB 5 --logLevel debug
  wait_for "$log" "End of Generation #0" 900
  sleep 5
  curl -s -X POST "localhost:$PORT/api/v1/writesOff" -H 'Content-Type: application/json' -d '{}' \
    > "$OUT_DIR/05-writesoff-api.txt" 2>&1
  if wait_for "$log" "Final generation done" 300; then
    record PASS writesoff "drain completed and final generation finished"
  else
    record FAIL writesoff "drain did not finish within 300s (possible hang)"
  fi
  mv_stop
}

t_resume () {
  local log="$OUT_DIR/06-resume.log" log2="$OUT_DIR/06-resume-restart.log"
  mv_run "$log" --srcNamespace "$SUITE_DB.crud" --dstNamespace "$SUITE_DB.crud" \
                --clean --start --logLevel info
  wait_for "$log" "End of Generation #0" 300
  mv_stop
  mv_run "$log2" --srcNamespace "$SUITE_DB.crud" --dstNamespace "$SUITE_DB.crud" \
                 --start --logLevel info
  sleep 50
  if grep -q "persisted resume token" "$log2" && ! grep -q "FTL" "$log2"; then
    record PASS resume "resumed from persisted token without error"
  else
    record FAIL resume "resume failed or errored"
  fi
  mv_stop
}

t_throughput () {
  local log="$OUT_DIR/07-throughput.log"
  local mk="const c=db.getSiblingDB('$SUITE_DB').hot; c.drop(); c.insertOne({_id:-1}); print('ready');"
  msh "$SRC_URI_SH" --eval "$mk" | tail -1
  msh "$DST_URI"    --eval "$mk" | tail -1
  mv_run "$log" --srcNamespace "$SUITE_DB.hot" --dstNamespace "$SUITE_DB.hot" \
                --clean --start --logLevel info
  wait_for "$log" "End of Generation #0" 300

  step "flooding source with 6 parallel batched writers for 60s"
  for w in 0 1 2 3 4 5; do
    msh "$SRC_URI_SH" --eval "
      const c=db.getSiblingDB('$SUITE_DB').hot; const BASE=$w*10000000; const END=Date.now()+60000;
      let n=0; while(Date.now()<END){const b=[];for(let i=0;i<500;i++)b.push({_id:BASE+n+i,p:'x'.repeat(80)});
      try{c.insertMany(b,{ordered:false});}catch(e){} n+=500;} print('WROTE '+n);" \
      > "$OUT_DIR/07-writer-$w.log" 2>&1 &
  done
  wait
  local written
  written=$(grep -h -oE 'WROTE [0-9]+' "$OUT_DIR"/07-writer-*.log | awk '{s+=$2} END{print s+0}')
  step "writers done: $written inserts; measuring reader drain"

  local consumed=0 prev=0 stalls=0 t0; t0=$(date +%s)
  for i in $(seq 1 240); do
    consumed=$(grep -E "^\| Source" "$log" | tail -1 | awk -F'|' '{gsub(/[ ,]/,"",$3); print $3}')
    consumed=${consumed:-0}
    [ "$consumed" -ge "$written" ] 2>/dev/null && break
    [ "$consumed" -eq "$prev" ] && stalls=$((stalls+1)) || stalls=0
    [ "$stalls" -ge 30 ] && break
    prev=$consumed; sleep 10
  done
  local elapsed=$(( $(date +%s) - t0 ))
  local rate=$(( elapsed > 0 ? consumed / elapsed : 0 ))
  {
    echo "writes accepted : $written in 60s (~$((written/60))/sec)"
    echo "events consumed : $consumed"
    echo "drain elapsed   : ${elapsed}s"
    echo "reader rate     : ~${rate} events/sec"
  } | tee "$OUT_DIR/07-throughput-summary.txt"
  if [ "$consumed" -ge "$written" ] 2>/dev/null; then
    record PASS throughput "consumed all $written events at ~${rate}/sec"
  else
    record FAIL throughput "consumed only $consumed of $written"
  fi
  mv_stop
}

t_verifyall () {
  local log="$OUT_DIR/08-verifyall.log"
  if [ "$CLUSTERWIDE_ENABLED" != "1" ]; then
    record SKIP verifyall "cluster-wide change streams could not be enabled"
    return
  fi
  # Both sides must hold the same collection set: a collection present on only
  # one side is fatal under --verifyAll, in either direction.
  msh "$SRC_URI_SH" --eval "db.getSiblingDB('$SUITE_DB').crud.drop(); db.getSiblingDB('$SUITE_DB').hot.drop();" >/dev/null 2>&1
  msh "$DST_URI"    --eval "db.getSiblingDB('$SUITE_DB').crud.drop(); db.getSiblingDB('$SUITE_DB').hot.drop();" >/dev/null 2>&1
  mv_run "$log" --verifyAll --clean --start --partitionSizeMB 5 --logLevel info
  if wait_for "$log" "End of Generation #0" 1200; then
    gen0_block "$log" > "$OUT_DIR/08-verifyall-gen0.txt"
    record PASS verifyall "$(grep -oE 'Namespaces compared: [0-9]+' "$OUT_DIR/08-verifyall-gen0.txt" | head -1)"
  else
    record FAIL verifyall "did not complete generation 0 within 1200s"
  fi
  mv_stop
}

t_scale () {
  local log="$OUT_DIR/09-scale.log"
  step "seeding $SCALE_COLLS x $SCALE_DOCS on both clusters"
  seed_both "$SCALE_DB" "$SCALE_DOCS" "$SCALE_COLLS"
  local args=()
  for c in $(seq 0 $((SCALE_COLLS-1))); do
    args+=(--srcNamespace "$SCALE_DB.alltypes$c" --dstNamespace "$SCALE_DB.alltypes$c")
  done
  mv_run "$log" "${args[@]}" --clean --start --partitionSizeMB 5 --logLevel info
  if wait_for "$log" "End of Generation #0" 2400; then
    gen0_block "$log" > "$OUT_DIR/09-scale-gen0.txt"
    if grep -q "No mismatches found" "$OUT_DIR/09-scale-gen0.txt"; then
      record PASS scale "$(grep -oE 'Source documents compared: [0-9,]+ \([0-9,.]+/sec\)' "$OUT_DIR/09-scale-gen0.txt" | head -1)"
    else
      record FAIL scale "mismatches reported on identical data"
    fi
  else
    record FAIL scale "did not complete generation 0 within 2400s"
  fi
  mv_stop
}

# =================================================================== main ===

for t in gates baseline mismatch crud writesoff resume throughput verifyall scale; do
  if want "$t"; then log "running: $t"; "t_$t"; fi
done

{
  echo "=============================================="
  echo " DocumentDB verification suite results"
  echo " $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "=============================================="
  for r in "${RESULTS[@]:-}"; do
    [ -z "$r" ] && continue
    IFS='|' read -r st name detail <<< "$r"
    printf '%-6s %-12s %s\n' "$st" "$name" "$detail"
  done
  echo "----------------------------------------------"
  echo "pass=$PASS fail=$FAIL skip=$SKIP"
} | tee "$OUT_DIR/RESULTS.txt"

echo
log "reviewing logs"
"$HERE/analyze_logs.sh" "$OUT_DIR" | tee "$OUT_DIR/ANALYSIS.txt"
ANALYSIS_RC=${PIPESTATUS[0]}

echo
log "logs and analysis: $OUT_DIR"
[ "$FAIL" -eq 0 ] && [ "$ANALYSIS_RC" -eq 0 ]
