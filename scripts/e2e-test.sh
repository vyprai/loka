#!/usr/bin/env bash
# ──────────────────────────────────────────────────────────────
#  LOKA — Comprehensive End-to-End Test Suite
# ──────────────────────────────────────────────────────────────
set -euo pipefail

LOKAD="${LOKAD:-/tmp/lokad}"
LOKACTL="${LOKACTL:-/tmp/loka}"
SERVER="http://localhost:8080"

PASS=0
FAIL=0
TOTAL=0

# ── Helpers ───────────────────────────────────────────────────

red()   { printf "\033[31m%s\033[0m" "$*"; }
green() { printf "\033[32m%s\033[0m" "$*"; }
bold()  { printf "\033[1m%s\033[0m" "$*"; }

assert_eq() {
  local label="$1" expected="$2" actual="$3"
  TOTAL=$((TOTAL + 1))
  if [ "$expected" = "$actual" ]; then
    PASS=$((PASS + 1))
    printf "  %-50s %s\n" "$label" "$(green PASS)"
  else
    FAIL=$((FAIL + 1))
    printf "  %-50s %s  (expected=%s got=%s)\n" "$label" "$(red FAIL)" "$expected" "$actual"
  fi
}

assert_contains() {
  local label="$1" needle="$2" haystack="$3"
  TOTAL=$((TOTAL + 1))
  if echo "$haystack" | grep -q "$needle"; then
    PASS=$((PASS + 1))
    printf "  %-50s %s\n" "$label" "$(green PASS)"
  else
    FAIL=$((FAIL + 1))
    printf "  %-50s %s  (missing: %s)\n" "$label" "$(red FAIL)" "$needle"
  fi
}

assert_not_contains() {
  local label="$1" needle="$2" haystack="$3"
  TOTAL=$((TOTAL + 1))
  if echo "$haystack" | grep -q "$needle"; then
    FAIL=$((FAIL + 1))
    printf "  %-50s %s  (found unwanted: %s)\n" "$label" "$(red FAIL)" "$needle"
  else
    PASS=$((PASS + 1))
    printf "  %-50s %s\n" "$label" "$(green PASS)"
  fi
}

api() {
  local method="$1" path="$2"
  shift 2
  curl -s -X "$method" "${SERVER}${path}" "$@"
}

api_json() {
  local method="$1" path="$2" body="$3"
  curl -s -X "$method" "${SERVER}${path}" -H "Content-Type: application/json" -d "$body"
}

jq_field() {
  python3 -c "import sys,json; print(json.load(sys.stdin)$1)" 2>/dev/null
}

cleanup() {
  pkill -f "$LOKAD" 2>/dev/null || true
  rm -rf /tmp/loka.db /tmp/loka-data
}

# ── Setup ─────────────────────────────────────────────────────

cleanup
sleep 0.3
cd /tmp && "$LOKAD" >/dev/null 2>&1 &
LOKAD_PID=$!
sleep 1.5

echo ""
bold "══════════════════════════════════════════════════════════"
bold "  LOKA — Comprehensive End-to-End Test Suite"
bold "══════════════════════════════════════════════════════════"
echo ""

# ══════════════════════════════════════════════════════════════
bold "1. HEALTH & STATUS"
echo "──────────────────────────────────────────────────────────"

HEALTH=$(api GET /api/v1/health)
assert_eq "health status ok" "ok" "$(echo "$HEALTH" | jq_field "['status']")"
assert_eq "workers_total >= 1" "True" "$(echo "$HEALTH" | python3 -c "import sys,json; print(json.load(sys.stdin)['workers_total'] >= 1)")"

echo ""

# ══════════════════════════════════════════════════════════════
bold "2. SESSION LIFECYCLE"
echo "──────────────────────────────────────────────────────────"

# Create
S1=$(api_json POST /api/v1/sessions '{"name":"lifecycle-test","mode":"execute"}')
S1_ID=$(echo "$S1" | jq_field "['ID']")
assert_eq "session created" "running" "$(echo "$S1" | jq_field "['Status']")"
assert_eq "session name" "lifecycle-test" "$(echo "$S1" | jq_field "['Name']")"
assert_eq "session mode" "execute" "$(echo "$S1" | jq_field "['Mode']")"

# Get
S1_GET=$(api GET "/api/v1/sessions/$S1_ID")
assert_eq "session get matches" "$S1_ID" "$(echo "$S1_GET" | jq_field "['ID']")"

# List
S_LIST=$(api GET /api/v1/sessions)
assert_eq "session list total >= 1" "True" "$(echo "$S_LIST" | python3 -c "import sys,json; print(json.load(sys.stdin)['total'] >= 1)")"

# Pause
S1_PAUSED=$(api_json POST "/api/v1/sessions/$S1_ID/pause" '{}')
assert_eq "session paused" "paused" "$(echo "$S1_PAUSED" | jq_field "['Status']")"

# Resume
S1_RESUMED=$(api_json POST "/api/v1/sessions/$S1_ID/resume" '{}')
assert_eq "session resumed" "running" "$(echo "$S1_RESUMED" | jq_field "['Status']")"

# Destroy
api DELETE "/api/v1/sessions/$S1_ID" >/dev/null
S1_AFTER=$(api GET "/api/v1/sessions/$S1_ID")
assert_eq "session terminated" "terminated" "$(echo "$S1_AFTER" | jq_field "['Status']")"

echo ""

# ══════════════════════════════════════════════════════════════
bold "3. COMMAND EXECUTION"
echo "──────────────────────────────────────────────────────────"

S2=$(api_json POST /api/v1/sessions '{"name":"exec-test","mode":"execute"}')
S2_ID=$(echo "$S2" | jq_field "['ID']")

# Single command
E1=$(api_json POST "/api/v1/sessions/$S2_ID/exec" '{"command":"echo","args":["hello-loka"]}')
E1_ID=$(echo "$E1" | jq_field "['ID']")
sleep 0.3
E1_GET=$(api GET "/api/v1/sessions/$S2_ID/exec/$E1_ID")
assert_eq "exec status success" "success" "$(echo "$E1_GET" | jq_field "['Status']")"
assert_contains "exec stdout has output" "hello-loka" "$(echo "$E1_GET" | jq_field "['Results']")"

# Command with args
E2=$(api_json POST "/api/v1/sessions/$S2_ID/exec" '{"command":"python3","args":["-c","print(2+2)"]}')
E2_ID=$(echo "$E2" | jq_field "['ID']")
sleep 0.3
E2_GET=$(api GET "/api/v1/sessions/$S2_ID/exec/$E2_ID")
assert_contains "python3 output" "4" "$(echo "$E2_GET" | jq_field "['Results']")"

# Parallel execution
E3=$(api_json POST "/api/v1/sessions/$S2_ID/exec" '{"commands":[{"id":"a","command":"echo","args":["par-A"]},{"id":"b","command":"echo","args":["par-B"]},{"id":"c","command":"echo","args":["par-C"]}],"parallel":true}')
E3_ID=$(echo "$E3" | jq_field "['ID']")
sleep 0.5
E3_GET=$(api GET "/api/v1/sessions/$S2_ID/exec/$E3_ID")
E3_RESULTS=$(echo "$E3_GET" | jq_field "['Results']")
assert_contains "parallel result A" "par-A" "$E3_RESULTS"
assert_contains "parallel result B" "par-B" "$E3_RESULTS"
assert_contains "parallel result C" "par-C" "$E3_RESULTS"

# Failing command
E4=$(api_json POST "/api/v1/sessions/$S2_ID/exec" '{"command":"false"}')
E4_ID=$(echo "$E4" | jq_field "['ID']")
sleep 0.3
E4_GET=$(api GET "/api/v1/sessions/$S2_ID/exec/$E4_ID")
assert_eq "failing command status" "failed" "$(echo "$E4_GET" | jq_field "['Status']")"

# List executions
E_LIST=$(api GET "/api/v1/sessions/$S2_ID/exec")
assert_eq "exec list >= 4" "True" "$(echo "$E_LIST" | python3 -c "import sys,json; print(json.load(sys.stdin)['total'] >= 4)")"

api DELETE "/api/v1/sessions/$S2_ID" >/dev/null
echo ""

# ══════════════════════════════════════════════════════════════
bold "4. EXECUTION MODES"
echo "──────────────────────────────────────────────────────────"

S3=$(api_json POST /api/v1/sessions '{"name":"mode-test","mode":"inspect"}')
S3_ID=$(echo "$S3" | jq_field "['ID']")

assert_eq "initial mode inspect" "inspect" "$(echo "$S3" | jq_field "['Mode']")"

# Transition: inspect -> plan
S3_PLAN=$(api_json POST "/api/v1/sessions/$S3_ID/mode" '{"mode":"plan"}')
assert_eq "mode -> plan" "plan" "$(echo "$S3_PLAN" | jq_field "['Mode']")"

# Transition: plan -> execute
S3_EXEC=$(api_json POST "/api/v1/sessions/$S3_ID/mode" '{"mode":"execute"}')
assert_eq "mode -> execute" "execute" "$(echo "$S3_EXEC" | jq_field "['Mode']")"

# Transition: execute -> commit
S3_COMMIT=$(api_json POST "/api/v1/sessions/$S3_ID/mode" '{"mode":"commit"}')
assert_eq "mode -> commit" "commit" "$(echo "$S3_COMMIT" | jq_field "['Mode']")"

# Transition: commit -> ask
S3_ASK=$(api_json POST "/api/v1/sessions/$S3_ID/mode" '{"mode":"ask"}')
assert_eq "mode -> ask" "ask" "$(echo "$S3_ASK" | jq_field "['Mode']")"

# Invalid transition: inspect -> commit (not allowed directly)
api_json POST "/api/v1/sessions/$S3_ID/mode" '{"mode":"inspect"}' >/dev/null
S3_BAD=$(api_json POST "/api/v1/sessions/$S3_ID/mode" '{"mode":"commit"}')
assert_contains "invalid transition blocked" "cannot transition" "$S3_BAD"

api DELETE "/api/v1/sessions/$S3_ID" >/dev/null
echo ""

# ══════════════════════════════════════════════════════════════
bold "5. EXEC POLICY — ALLOWED COMMANDS"
echo "──────────────────────────────────────────────────────────"

S4=$(api_json POST /api/v1/sessions '{"name":"policy-allow","mode":"execute","allowed_commands":["echo","ls","cat"]}')
S4_ID=$(echo "$S4" | jq_field "['ID']")

# Allowed
P1=$(api_json POST "/api/v1/sessions/$S4_ID/exec" '{"command":"echo","args":["policy-ok"]}')
assert_not_contains "echo allowed" "error" "$P1"

# Blocked (not in allowlist)
P2=$(api_json POST "/api/v1/sessions/$S4_ID/exec" '{"command":"curl"}')
assert_contains "curl blocked by allowlist" "not in allowed list" "$P2"

P3=$(api_json POST "/api/v1/sessions/$S4_ID/exec" '{"command":"rm","args":["-rf","/"]}')
assert_contains "rm blocked by allowlist" "not in allowed list" "$P3"

api DELETE "/api/v1/sessions/$S4_ID" >/dev/null
echo ""

# ══════════════════════════════════════════════════════════════
bold "6. EXEC POLICY — BLOCKED COMMANDS"
echo "──────────────────────────────────────────────────────────"

S5=$(api_json POST /api/v1/sessions '{"name":"policy-block","mode":"execute","blocked_commands":["rm","dd","mkfs"]}')
S5_ID=$(echo "$S5" | jq_field "['ID']")

# Allowed (not blocked)
B1=$(api_json POST "/api/v1/sessions/$S5_ID/exec" '{"command":"echo","args":["ok"]}')
assert_not_contains "echo not blocked" "error" "$B1"

# Blocked
B2=$(api_json POST "/api/v1/sessions/$S5_ID/exec" '{"command":"rm"}')
assert_contains "rm blocked" "blocked by policy" "$B2"

B3=$(api_json POST "/api/v1/sessions/$S5_ID/exec" '{"command":"dd"}')
assert_contains "dd blocked" "blocked by policy" "$B3"

api DELETE "/api/v1/sessions/$S5_ID" >/dev/null
echo ""

# ══════════════════════════════════════════════════════════════
bold "7. EXEC POLICY — INSPECT MODE READ-ONLY"
echo "──────────────────────────────────────────────────────────"

S6=$(api_json POST /api/v1/sessions '{"name":"readonly-test","mode":"inspect"}')
S6_ID=$(echo "$S6" | jq_field "['ID']")

# Read-only commands allowed
R1=$(api_json POST "/api/v1/sessions/$S6_ID/exec" '{"command":"echo","args":["readonly"]}')
assert_not_contains "echo in inspect" "error" "$R1"

R2=$(api_json POST "/api/v1/sessions/$S6_ID/exec" '{"command":"ls","args":["/"]}')
assert_not_contains "ls in inspect" "error" "$R2"

# Write commands blocked
R3=$(api_json POST "/api/v1/sessions/$S6_ID/exec" '{"command":"cp"}')
assert_contains "cp blocked in inspect" "not allowed in read-only" "$R3"

R4=$(api_json POST "/api/v1/sessions/$S6_ID/exec" '{"command":"mkdir"}')
assert_contains "mkdir blocked in inspect" "not allowed in read-only" "$R4"

api DELETE "/api/v1/sessions/$S6_ID" >/dev/null
echo ""

# ══════════════════════════════════════════════════════════════
bold "8. EXEC POLICY — ASK MODE (APPROVAL FLOW)"
echo "──────────────────────────────────────────────────────────"

S7=$(api_json POST /api/v1/sessions '{"name":"ask-test","mode":"ask"}')
S7_ID=$(echo "$S7" | jq_field "['ID']")

# Submit — should return pending_approval
A1=$(api_json POST "/api/v1/sessions/$S7_ID/exec" '{"command":"echo","args":["needs-approval"]}')
A1_ID=$(echo "$A1" | jq_field "['ID']")
assert_eq "ask mode -> pending_approval" "pending_approval" "$(echo "$A1" | jq_field "['Status']")"

# Approve
A1_APPROVED=$(api_json POST "/api/v1/sessions/$S7_ID/exec/$A1_ID/approve" '{}')
assert_eq "approved -> running" "running" "$(echo "$A1_APPROVED" | jq_field "['Status']")"
# Wait for the suspended goroutine to resume and execute.
for i in $(seq 1 20); do
  sleep 0.2
  A1_STATUS=$(api GET "/api/v1/sessions/$S7_ID/exec/$A1_ID" | jq_field "['Status']")
  [ "$A1_STATUS" = "success" ] && break
done
A1_DONE=$(api GET "/api/v1/sessions/$S7_ID/exec/$A1_ID")
assert_eq "approved exec completes" "success" "$(echo "$A1_DONE" | jq_field "['Status']")"

# Submit another — reject it
A2=$(api_json POST "/api/v1/sessions/$S7_ID/exec" '{"command":"echo","args":["reject-me"]}')
A2_ID=$(echo "$A2" | jq_field "['ID']")
assert_eq "pending before reject" "pending_approval" "$(echo "$A2" | jq_field "['Status']")"

A2_REJECTED=$(api_json POST "/api/v1/sessions/$S7_ID/exec/$A2_ID/reject" '{"reason":"too dangerous"}')
assert_eq "rejected status" "rejected" "$(echo "$A2_REJECTED" | jq_field "['Status']")"

api DELETE "/api/v1/sessions/$S7_ID" >/dev/null
echo ""

# ══════════════════════════════════════════════════════════════
bold "9. CHECKPOINTS"
echo "──────────────────────────────────────────────────────────"

S8=$(api_json POST /api/v1/sessions '{"name":"checkpoint-test","mode":"execute"}')
S8_ID=$(echo "$S8" | jq_field "['ID']")
WS="/tmp/loka-data/artifacts/worker-data/sessions/$S8_ID/workspace"

# Write files
api_json POST "/api/v1/sessions/$S8_ID/exec" "{\"command\":\"sh\",\"args\":[\"-c\",\"echo v1 > $WS/data.txt && echo config > $WS/config.txt\"]}" >/dev/null
sleep 0.3

# Checkpoint A
CPA=$(api_json POST "/api/v1/sessions/$S8_ID/checkpoints" '{"type":"light","label":"state-A"}')
CPA_ID=$(echo "$CPA" | jq_field "['ID']")
sleep 0.5
CPA_GET=$(api GET "/api/v1/sessions/$S8_ID/checkpoints" | python3 -c "import sys,json; cps=json.load(sys.stdin)['checkpoints']; print(len(cps))")
assert_eq "checkpoint A created" "1" "$CPA_GET"

# Modify files
api_json POST "/api/v1/sessions/$S8_ID/exec" "{\"command\":\"sh\",\"args\":[\"-c\",\"echo v2-MODIFIED > $WS/data.txt && rm -f $WS/config.txt && echo new > $WS/extra.txt\"]}" >/dev/null
sleep 0.3

# Checkpoint B
CPB=$(api_json POST "/api/v1/sessions/$S8_ID/checkpoints" '{"type":"full","label":"state-B"}')
CPB_ID=$(echo "$CPB" | jq_field "['ID']")
sleep 0.5

# Verify checkpoint DAG
CP_LIST=$(api GET "/api/v1/sessions/$S8_ID/checkpoints")
CP_COUNT=$(echo "$CP_LIST" | python3 -c "import sys,json; print(len(json.load(sys.stdin)['checkpoints']))")
assert_eq "checkpoint count = 2" "2" "$CP_COUNT"

# Verify modified state
V1=$(api_json POST "/api/v1/sessions/$S8_ID/exec" "{\"command\":\"cat\",\"args\":[\"$WS/data.txt\"]}" | jq_field "['ID']")
sleep 0.3
V1_OUT=$(api GET "/api/v1/sessions/$S8_ID/exec/$V1" | python3 -c "import sys,json; print(json.load(sys.stdin)['Results'][0]['Stdout'].strip())")
assert_eq "data.txt is v2 before restore" "v2-MODIFIED" "$V1_OUT"

# Restore to A
api_json POST "/api/v1/sessions/$S8_ID/checkpoints/$CPA_ID/restore" '{}' >/dev/null
sleep 0.5

# Verify restored state
V2=$(api_json POST "/api/v1/sessions/$S8_ID/exec" "{\"command\":\"cat\",\"args\":[\"$WS/data.txt\"]}" | jq_field "['ID']")
sleep 0.3
V2_OUT=$(api GET "/api/v1/sessions/$S8_ID/exec/$V2" | python3 -c "import sys,json; print(json.load(sys.stdin)['Results'][0]['Stdout'].strip())")
assert_eq "data.txt restored to v1" "v1" "$V2_OUT"

V3=$(api_json POST "/api/v1/sessions/$S8_ID/exec" "{\"command\":\"cat\",\"args\":[\"$WS/config.txt\"]}" | jq_field "['ID']")
sleep 0.3
V3_OUT=$(api GET "/api/v1/sessions/$S8_ID/exec/$V3" | python3 -c "import sys,json; print(json.load(sys.stdin)['Results'][0]['Stdout'].strip())")
assert_eq "config.txt restored" "config" "$V3_OUT"

V4=$(api_json POST "/api/v1/sessions/$S8_ID/exec" "{\"command\":\"sh\",\"args\":[\"-c\",\"test -f $WS/extra.txt && echo YES || echo NO\"]}" | jq_field "['ID']")
sleep 0.3
V4_OUT=$(api GET "/api/v1/sessions/$S8_ID/exec/$V4" | python3 -c "import sys,json; print(json.load(sys.stdin)['Results'][0]['Stdout'].strip())")
assert_eq "extra.txt gone after restore" "NO" "$V4_OUT"

# Delete checkpoint subtree
api DELETE "/api/v1/sessions/$S8_ID/checkpoints/$CPB_ID" >/dev/null
CP_AFTER=$(api GET "/api/v1/sessions/$S8_ID/checkpoints" | python3 -c "import sys,json; print(len(json.load(sys.stdin)['checkpoints']))")
assert_eq "checkpoint B deleted" "1" "$CP_AFTER"

api DELETE "/api/v1/sessions/$S8_ID" >/dev/null
echo ""

# ══════════════════════════════════════════════════════════════
bold "10. WORKERS"
echo "──────────────────────────────────────────────────────────"

W_LIST=$(api GET /api/v1/workers)
W_COUNT=$(echo "$W_LIST" | python3 -c "import sys,json; print(json.load(sys.stdin)['total'])")
assert_eq "workers >= 1" "True" "$(python3 -c "print($W_COUNT >= 1)")"

W_ID=$(echo "$W_LIST" | python3 -c "import sys,json; print(json.load(sys.stdin)['workers'][0]['ID'])")

# Get worker
W_GET=$(api GET "/api/v1/workers/$W_ID")
assert_eq "worker provider" "local" "$(echo "$W_GET" | jq_field "['Provider']")"

# Label worker
W_LABELED=$(api_json PUT "/api/v1/workers/$W_ID/labels" '{"labels":{"gpu":"true","tier":"premium"}}')
assert_contains "label applied" "gpu" "$W_LABELED"

# Drain
W_DRAINED=$(api_json POST "/api/v1/workers/$W_ID/drain" '{"timeout_seconds":60}')
assert_eq "worker draining" "draining" "$(echo "$W_DRAINED" | jq_field "['Status']")"

# Undrain
W_UNDRAINED=$(api_json POST "/api/v1/workers/$W_ID/undrain" '{}')
assert_eq "worker undrained" "ready" "$(echo "$W_UNDRAINED" | jq_field "['Status']")"

echo ""

# ══════════════════════════════════════════════════════════════
bold "11. PROVIDERS"
echo "──────────────────────────────────────────────────────────"

PROV_LIST=$(api GET /api/v1/providers)
PROV_COUNT=$(echo "$PROV_LIST" | python3 -c "import sys,json; print(len(json.load(sys.stdin)['providers']))")
assert_eq "providers = 7" "7" "$PROV_COUNT"

PROV_NAMES=$(echo "$PROV_LIST" | python3 -c "import sys,json; print(','.join(sorted(p['name'] for p in json.load(sys.stdin)['providers'])))")
assert_contains "has aws" "aws" "$PROV_NAMES"
assert_contains "has gcp" "gcp" "$PROV_NAMES"
assert_contains "has azure" "azure" "$PROV_NAMES"
assert_contains "has digitalocean" "digitalocean" "$PROV_NAMES"
assert_contains "has ovh" "ovh" "$PROV_NAMES"
assert_contains "has local" "local" "$PROV_NAMES"
assert_contains "has selfmanaged" "selfmanaged" "$PROV_NAMES"

echo ""

# ══════════════════════════════════════════════════════════════
bold "12. WORKER TOKENS"
echo "──────────────────────────────────────────────────────────"

# Create
T1=$(api_json POST /api/v1/worker-tokens '{"name":"server-1","expires_seconds":3600}')
T1_ID=$(echo "$T1" | jq_field "['ID']")
T1_TOK=$(echo "$T1" | jq_field "['Token']")
assert_contains "token has loka_ prefix" "loka_" "$T1_TOK"

T2=$(api_json POST /api/v1/worker-tokens '{"name":"server-2","expires_seconds":7200}')
T2_ID=$(echo "$T2" | jq_field "['ID']")

# List
T_LIST=$(api GET /api/v1/worker-tokens)
T_COUNT=$(echo "$T_LIST" | python3 -c "import sys,json; print(json.load(sys.stdin)['total'])")
assert_eq "token count = 2" "2" "$T_COUNT"

# Revoke
api DELETE "/api/v1/worker-tokens/$T1_ID" >/dev/null
T_AFTER=$(api GET /api/v1/worker-tokens | python3 -c "import sys,json; print(json.load(sys.stdin)['total'])")
assert_eq "token count after revoke = 1" "1" "$T_AFTER"

api DELETE "/api/v1/worker-tokens/$T2_ID" >/dev/null
echo ""

# ══════════════════════════════════════════════════════════════
bold "13. PACKAGES"
echo "──────────────────────────────────────────────────────────"

# Install
api_json POST /api/v1/packages/install '{"name":"python@3.12"}' >/dev/null
api_json POST /api/v1/packages/install '{"name":"git"}' >/dev/null
api_json POST /api/v1/packages/install '{"name":"ripgrep"}' >/dev/null

PKG_LIST=$(api GET /api/v1/packages)
PKG_COUNT=$(echo "$PKG_LIST" | python3 -c "import sys,json; pkgs=json.load(sys.stdin)['packages']; print(len(pkgs) if pkgs else 0)")
assert_eq "packages installed = 3" "3" "$PKG_COUNT"

# Get
PKG_PY=$(api GET "/api/v1/packages/python@3.12")
assert_eq "python version" "3.12" "$(echo "$PKG_PY" | jq_field "['Version']")"

# Validate
PKG_VAL=$(api_json POST /api/v1/packages/validate '{"packages":["python@3.12","ripgrep","unknown-tool"]}')
assert_eq "python valid" "True" "$(echo "$PKG_VAL" | python3 -c "import sys,json; print(json.load(sys.stdin)['results']['python@3.12'])")"
assert_eq "unknown invalid" "False" "$(echo "$PKG_VAL" | python3 -c "import sys,json; print(json.load(sys.stdin)['results']['unknown-tool'])")"

# Search
PKG_SEARCH=$(api GET "/api/v1/packages?search=python")
PKG_SEARCH_COUNT=$(echo "$PKG_SEARCH" | python3 -c "import sys,json; pkgs=json.load(sys.stdin)['packages']; print(len(pkgs) if pkgs else 0)")
assert_eq "search python = 1" "1" "$PKG_SEARCH_COUNT"

# Remove
api DELETE "/api/v1/packages/ripgrep" >/dev/null
PKG_AFTER=$(api GET /api/v1/packages | python3 -c "import sys,json; pkgs=json.load(sys.stdin)['packages']; print(len(pkgs) if pkgs else 0)")
assert_eq "packages after remove = 2" "2" "$PKG_AFTER"

echo ""

# ══════════════════════════════════════════════════════════════
bold "14. PACKAGE PROFILES"
echo "──────────────────────────────────────────────────────────"

# Create
api_json POST /api/v1/package-profiles '{"name":"data-agent","packages":["python@3.12","git"]}' >/dev/null
api_json POST /api/v1/package-profiles '{"name":"web-agent","packages":["git"]}' >/dev/null

PROF_LIST=$(api GET /api/v1/package-profiles)
PROF_COUNT=$(echo "$PROF_LIST" | python3 -c "import sys,json; profs=json.load(sys.stdin)['profiles']; print(len(profs) if profs else 0)")
assert_eq "profiles = 2" "2" "$PROF_COUNT"

# Get
PROF_DA=$(api GET "/api/v1/package-profiles/data-agent")
assert_eq "profile name" "data-agent" "$(echo "$PROF_DA" | jq_field "['Name']")"

# Update
PROF_UPD=$(api_json PUT "/api/v1/package-profiles/data-agent" '{"add":["ripgrep"],"remove":["git"]}')
PROF_UPD_PKGS=$(echo "$PROF_UPD" | python3 -c "import sys,json; print(','.join(sorted(json.load(sys.stdin)['Packages'])))")
assert_contains "profile has ripgrep" "ripgrep" "$PROF_UPD_PKGS"
assert_not_contains "profile no git" "git" "$PROF_UPD_PKGS"

# Delete
api DELETE "/api/v1/package-profiles/web-agent" >/dev/null
PROF_AFTER=$(api GET /api/v1/package-profiles | python3 -c "import sys,json; profs=json.load(sys.stdin)['profiles']; print(len(profs) if profs else 0)")
assert_eq "profiles after delete = 1" "1" "$PROF_AFTER"

echo ""

# ══════════════════════════════════════════════════════════════
bold "15. PROMETHEUS METRICS"
echo "──────────────────────────────────────────────────────────"

api GET /api/v1/health >/dev/null  # Ensure at least one request for latency metric.
METRICS=$(api GET /metrics)
assert_contains "has api_requests metric" "loka_api_requests_total" "$METRICS"
assert_contains "has api_latency metric" "loka_api_latency" "$METRICS"
assert_contains "has sessions_created metric" "loka_sessions_created_total" "$METRICS"
assert_contains "has executions metric" "loka_executions_total" "$METRICS"

echo ""

# ══════════════════════════════════════════════════════════════
bold "16. CLI SMOKE TEST"
echo "──────────────────────────────────────────────────────────"

CLI_VER=$("$LOKACTL" version 2>&1)
assert_contains "cli version output" "loka" "$CLI_VER"

CLI_STATUS=$("$LOKACTL" status 2>&1)
assert_contains "cli status shows control plane" "Control Plane" "$CLI_STATUS"

CLI_WORKERS=$("$LOKACTL" worker list 2>&1)
assert_contains "cli worker list has header" "HOSTNAME" "$CLI_WORKERS"

CLI_PROVIDERS=$("$LOKACTL" provider list 2>&1)
assert_contains "cli provider list has aws" "aws" "$CLI_PROVIDERS"

CLI_HELP=$("$LOKACTL" --help 2>&1)
assert_contains "cli has session cmd" "session" "$CLI_HELP"
assert_contains "cli has exec cmd" "exec" "$CLI_HELP"
assert_contains "cli has checkpoint cmd" "checkpoint" "$CLI_HELP"
assert_contains "cli has worker cmd" "worker" "$CLI_HELP"
assert_contains "cli has package cmd" "package" "$CLI_HELP"
assert_contains "cli has profile cmd" "profile" "$CLI_HELP"
assert_contains "cli has provider cmd" "provider" "$CLI_HELP"
assert_contains "cli has token cmd" "token" "$CLI_HELP"

echo ""

# ══════════════════════════════════════════════════════════════
# Summary
# ══════════════════════════════════════════════════════════════

cleanup

echo ""
bold "══════════════════════════════════════════════════════════"
if [ "$FAIL" -eq 0 ]; then
  bold "  RESULT: $(green "ALL $TOTAL TESTS PASSED")"
else
  bold "  RESULT: $(green "$PASS passed"), $(red "$FAIL failed") out of $TOTAL"
fi
bold "══════════════════════════════════════════════════════════"
echo ""

exit "$FAIL"
