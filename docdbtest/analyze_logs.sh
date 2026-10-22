#!/usr/bin/env bash
#
# Reviews logs produced by run_suite.sh and reports anything wrong.
#
#   ./analyze_logs.sh <log-dir>
#
# Findings are graded:
#   CRITICAL - a wrong or untrustworthy verification result, or a crash
#   WARN     - works, but degraded or worth understanding
#   INFO     - context
#
# The signatures below are the concrete failures observed while bringing up
# DocumentDB support; each one cost a debugging session to find the first time.
#
set -uo pipefail

DIR="${1:-}"
[ -d "$DIR" ] || { echo "usage: $0 <log-dir>" >&2; exit 2; }

CRIT=0; WARNS=0

crit () { echo "CRITICAL  $1"; [ -n "${2:-}" ] && echo "          $2"; CRIT=$((CRIT+1)); }
warn () { echo "WARN      $1"; [ -n "${2:-}" ] && echo "          $2"; WARNS=$((WARNS+1)); }
info () { echo "INFO      $1"; }

# Search everything except the gates log, where rejections are the point.
logs () { find "$DIR" -name '*.log' ! -name '01-gates.log' -print0; }
hits () { xargs -0 grep -hoE "$1" 2>/dev/null < <(logs) | head -"${2:-3}"; }
count () { xargs -0 grep -hcE "$1" 2>/dev/null < <(logs) | awk '{s+=$1} END{print s+0}'; }

echo "=============================================="
echo " Log review: $DIR"
echo "=============================================="
echo

# ---------------------------------------------------- crashes and aborts ---

if [ "$(count 'panic:|nil pointer dereference')" -gt 0 ]; then
  crit "verifier panicked" "$(hits 'panic: [^\"]{0,80}' 2)"
fi

if [ "$(count "Feature not supported: 'causal consistency'")" -gt 0 ]; then
  crit "causal-consistency error (code 303)" \
       "sessions must disable causal consistency for DocumentDB; this usually appears only on a change-stream resume, e.g. after a failover"
fi

if [ "$(count 'Feature not supported: \$\$REMOVE')" -gt 0 ]; then
  crit "change stream pipeline used \$\$REMOVE" \
       "the DocumentDB pipeline branch is not being taken -- check flavor detection"
fi

if [ "$(count 'unexpected .v. value')" -gt 0 ]; then
  crit "index comparison rejected an index version" \
       "DocumentDB reports v:4; normalizeIndexSpecForDocumentDB should rewrite it"
fi

if [ "$(count 'CappedPositionLost|ChangeStreamHistoryLost')" -gt 0 ]; then
  crit "resume token expired -- the run cannot be trusted and must restart with --clean" \
       "raise change_stream_log_retention_duration (default 3h)"
fi

if [ "$(count 'unknown optype')" -gt 0 ]; then
  warn "a DDL event aborted the run" "$(hits 'unknown optype[^\"]{0,60}' 1)"
fi

if [ "$(count 'Retryable writes are not supported')" -gt 0 ]; then
  warn "retryable writes attempted against DocumentDB" "add retryWrites=false to the source URI"
fi

FTL=$(count '\bFTL\b|Fatal error')
[ "$FTL" -gt 0 ] && crit "$FTL fatal error line(s)" "$(hits '"error":"[^"]{0,140}' 2)"

NR=$(count 'Non-retryable error')
[ "$NR" -gt 0 ] && warn "$NR non-retryable error line(s)" "$(hits 'Non-retryable error[^\"]{0,100}' 1)"

# ------------------------------------------- silently-wrong-result checks ---
#
# The worst outcome is a clean report over data that was never compared. That
# happened for real: an unequal `capped` value made the collection look
# uncomparable, so generation 0 skipped every document and still reported only
# a metadata mismatch.

for f in "$DIR"/*.log; do
  [ -f "$f" ] || continue
  case "$(basename "$f")" in 01-gates.log|07-writer-*.log) continue ;; esac
  n=$(grep -n "End of Generation #0" "$f" 2>/dev/null | head -1 | cut -d: -f1)
  [ -n "$n" ] || continue
  block=$(sed -n "${n},$((n+14))p" "$f")
  compared=$(echo "$block" | grep -oE 'Source documents compared: [0-9,]+' | head -1 | grep -oE '[0-9,]+' | tr -d ,)
  clean=$(echo "$block" | grep -c "No mismatches found")
  if [ "${compared:-0}" -eq 0 ] && [ "$clean" -gt 0 ]; then
    crit "$(basename "$f"): reported clean having compared ZERO documents" \
         "this is the silent-skip failure mode -- check collection option normalisation"
  fi
done

if [ "$(count 'exists only on (source|destination)')" -gt 0 ]; then
  warn "a collection exists on only one side" \
       "fatal in both directions under --verifyAll; $(hits 'exists only on [a-z]+' 1)"
fi

# ------------------------------------------------------------ degradation ---

if [ "$(count 'no operationTime')" -gt 0 ]; then
  warn "server reported no operationTime" "change-reader lag will be unavailable; writes-off may fail"
fi

if [ "$(count 'Comparing a capped collection')" -gt 0 ]; then
  info "capped collection compared in _id order (DocumentDB has no \$natural)"
fi

SAT=$(xargs -0 grep -hoE 'buffer is [0-9.]+% full' 2>/dev/null < <(logs) | sort -t' ' -k3 -rn | head -1)
[ -n "$SAT" ] && warn "change-reader buffer saturation observed: $SAT" \
                      "the reader is falling behind the write rate"

LAG=$(xargs -0 grep -hoE 'lag[^,;]{0,30}' 2>/dev/null < <(logs) | grep -vi 'no lag' | tail -1)
[ -n "$LAG" ] && info "reader lag reported: $LAG"

# ------------------------------------------------------------- throughput ---

if [ -f "$DIR/07-throughput-summary.txt" ]; then
  echo
  echo "--- throughput ---"
  cat "$DIR/07-throughput-summary.txt"
  rate=$(grep -oE 'reader rate *: ~[0-9]+' "$DIR/07-throughput-summary.txt" | grep -oE '[0-9]+$')
  wr=$(grep -oE '~[0-9]+/sec' "$DIR/07-throughput-summary.txt" | head -1 | grep -oE '[0-9]+')
  if [ -n "${rate:-}" ] && [ -n "${wr:-}" ] && [ "$rate" -gt 0 ]; then
    ratio=$(( wr / rate ))
    if [ "$ratio" -ge 5 ]; then
      warn "reader is ~${ratio}x slower than the accepted write rate" \
           "sustained writes near that rate will prevent convergence and may outrun the 3h retention window"
    else
      info "reader keeps within ${ratio}x of the write rate"
    fi
  fi
fi

# ---------------------------------------------------------------- results ---

if [ -f "$DIR/RESULTS.txt" ]; then
  echo
  echo "--- suite results ---"
  cat "$DIR/RESULTS.txt"
  grep -q '^FAIL' "$DIR/RESULTS.txt" && crit "one or more suite tests failed (see above)"
fi

echo
echo "=============================================="
echo " critical=$CRIT  warnings=$WARNS"
if [ "$CRIT" -eq 0 ] && [ "$WARNS" -eq 0 ]; then
  echo " nothing wrong found"
elif [ "$CRIT" -eq 0 ]; then
  echo " no critical findings; review the warnings above"
else
  echo " CRITICAL findings present -- do not trust these results"
fi
echo "=============================================="
[ "$CRIT" -eq 0 ]
