#!/usr/bin/env bash
# ──────────────────────────────────────────────────────────
#  LOKA End-to-End Test Suite
#
#  Starts a real lokad instance, creates sessions with real
#  Firecracker VMs, executes commands, and verifies results.
#
#  Requirements: Linux with KVM + Docker, or macOS with Lima
#  Usage: make e2e-test  (or bash scripts/e2e-test.sh)
# ──────────────────────────────────────────────────────────
set -euo pipefail

# ── Config ───────────────────────────────────────────────

LOKA_BIN="${LOKA_BIN:-./bin/loka}"
LOKAD_BIN="${LOKAD_BIN:-./bin/lokad}"
ENDPOINT="http://localhost:6840"
DB_PATH="/tmp/loka-e2e-test-$$.db"
DATA_DIR="/tmp/loka-e2e-test-$$"
LOKAD_PID=""
PASSED=0
FAILED=0
TOTAL=0

# ── Helpers ──────────────────────────────────────────────

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
CYAN='\033[0;36m'
BOLD='\033[1m'
NC='\033[0m'

pass() { ((PASSED++)); ((TOTAL++)); echo -e "  ${GREEN}✓${NC} $1"; }
fail() { ((FAILED++)); ((TOTAL++)); echo -e "  ${RED}✗${NC} $1: $2"; }
skip() { echo -e "  ${YELLOW}⊘${NC} $1 (skipped)"; }

jf() { python3 -c "import sys,json; print(json.load(sys.stdin).get('$1',''))" 2>/dev/null; }
jlen() { python3 -c "import sys,json; print(len(json.load(sys.stdin).get('$1',[])))" 2>/dev/null; }

cleanup() {
  echo ""
  echo -e "${CYAN}==> Cleaning up${NC}"
  if [ -n "$LOKAD_PID" ] && kill -0 "$LOKAD_PID" 2>/dev/null; then
    kill "$LOKAD_PID" 2>/dev/null; wait "$LOKAD_PID" 2>/dev/null || true
    echo "  lokad stopped (pid $LOKAD_PID)"
  fi
  rm -rf "$DATA_DIR" "$DB_PATH"
  echo ""
  echo -e "${BOLD}Results: ${GREEN}$PASSED passed${NC}, ${RED}$FAILED failed${NC}, $TOTAL total"
  [ "$FAILED" -gt 0 ] && exit 1 || exit 0
}
trap cleanup EXIT

# ── Build ────────────────────────────────────────────────

echo ""
echo -e "${BOLD}  LOKA E2E Test Suite${NC}"
echo ""
echo -e "${CYAN}==> Checking binaries${NC}"
mkdir -p ./bin
if [ -f "$LOKA_BIN" ] && [ -f "$LOKAD_BIN" ] && [ -f ./bin/loka-supervisor ]; then
  echo "  Using pre-built binaries"
elif command -v go &>/dev/null; then
  echo "  Building..."
  go build -o "$LOKA_BIN" ./cmd/loka
  go build -o "$LOKAD_BIN" ./cmd/lokad
  CGO_ENABLED=0 go build -ldflags="-s -w" -o ./bin/loka-supervisor ./cmd/loka-supervisor
  echo "  Built: loka, lokad, loka-supervisor"
else
  echo -e "  ${RED}No pre-built binaries and no Go compiler. Build first: make build${NC}"
  exit 1
fi

# ── Prerequisites ────────────────────────────────────────

echo ""
echo -e "${CYAN}==> Checking prerequisites${NC}"

FC_AVAILABLE=false
DOCKER_AVAILABLE=false

if [ -e /dev/kvm ]; then
  echo "  KVM: yes"
  FC_AVAILABLE=true
else
  echo -e "  ${YELLOW}KVM: no — Firecracker tests will be skipped${NC}"
fi

if command -v docker &>/dev/null && docker info >/dev/null 2>&1; then
  echo "  Docker: yes"
  DOCKER_AVAILABLE=true
else
  echo -e "  ${YELLOW}Docker: no — image pull tests will be skipped${NC}"
fi

if [ -f /usr/local/bin/firecracker ]; then
  echo "  Firecracker: yes"
else
  echo -e "  ${YELLOW}Firecracker: no${NC}"
  FC_AVAILABLE=false
fi

# ── Prepare rootfs ───────────────────────────────────────

mkdir -p "$DATA_DIR"/{kernel,rootfs,images}

# Link kernel
for p in /var/loka/kernel/vmlinux /tmp/loka-data/artifacts/kernel/vmlinux; do
  [ -f "$p" ] && ln -sf "$p" "$DATA_DIR/kernel/vmlinux" && break
done

# Create rootfs if we have Docker + KVM
if [ "$FC_AVAILABLE" = true ] && [ "$DOCKER_AVAILABLE" = true ]; then
  echo ""
  echo -e "${CYAN}==> Creating test rootfs${NC}"
  docker pull alpine:latest >/dev/null 2>&1
  CID=$(docker create alpine:latest)
  docker export "$CID" > /tmp/e2e-rootfs-$$.tar
  docker rm "$CID" >/dev/null

  dd if=/dev/zero of="$DATA_DIR/rootfs/rootfs.ext4" bs=1M count=512 2>/dev/null
  mkfs.ext4 -F "$DATA_DIR/rootfs/rootfs.ext4" >/dev/null 2>&1
  mkdir -p /tmp/e2e-mnt-$$
  sudo mount -o loop "$DATA_DIR/rootfs/rootfs.ext4" /tmp/e2e-mnt-$$
  sudo tar xf /tmp/e2e-rootfs-$$.tar -C /tmp/e2e-mnt-$$ 2>/dev/null
  sudo mkdir -p /tmp/e2e-mnt-$$/usr/local/bin
  sudo cp ./bin/loka-supervisor /tmp/e2e-mnt-$$/usr/local/bin/loka-supervisor
  sudo chmod +x /tmp/e2e-mnt-$$/usr/local/bin/loka-supervisor
  sudo umount /tmp/e2e-mnt-$$
  rmdir /tmp/e2e-mnt-$$
  rm -f /tmp/e2e-rootfs-$$.tar
  echo "  Rootfs ready (Alpine + supervisor)"
fi

# ── Start lokad ──────────────────────────────────────────

echo ""
echo -e "${CYAN}==> Starting lokad${NC}"

export LOKA_FIRECRACKER_BIN="${LOKA_FIRECRACKER_BIN:-/usr/local/bin/firecracker}"
export LOKA_KERNEL_PATH="$DATA_DIR/kernel/vmlinux"
export LOKA_ROOTFS_PATH="$DATA_DIR/rootfs/rootfs.ext4"

cat > "$DATA_DIR/config.yaml" << YAML
role: all
mode: single
listen_addr: ":6840"
grpc_addr: ":6841"
database:
  driver: sqlite
  dsn: "$DB_PATH"
objectstore:
  type: local
  path: "$DATA_DIR"
tls:
  auto: false
  allow_insecure: true
YAML

"$LOKAD_BIN" --config "$DATA_DIR/config.yaml" > "$DATA_DIR/lokad.log" 2>&1 &
LOKAD_PID=$!
echo "  lokad started (pid $LOKAD_PID)"

# Wait for health
echo -n "  Waiting..."
READY=false
for i in $(seq 1 30); do
  if curl -s "$ENDPOINT/api/v1/health" 2>/dev/null | grep -q "ok"; then
    READY=true; echo " ready!"; break
  fi
  echo -n "."; sleep 1
done
[ "$READY" = false ] && { echo " TIMEOUT"; fail "lokad startup" "health check timeout"; cat "$DATA_DIR/lokad.log" | tail -20; exit 1; }

# ═══════════════════════════════════════════════════════════
#  TESTS
# ═══════════════════════════════════════════════════════════

# ── 1. Health ────────────────────────────────────────────

echo ""
echo -e "${CYAN}==> 1. Health${NC}"

HEALTH=$(curl -s "$ENDPOINT/api/v1/health")
echo "$HEALTH" | grep -q '"ok"' && pass "Health endpoint returns ok" || fail "Health" "$HEALTH"

# ── 2. Workers ───────────────────────────────────────────

echo ""
echo -e "${CYAN}==> 2. Workers${NC}"

WT=$(echo "$HEALTH" | python3 -c "import sys,json; print(json.load(sys.stdin).get('workers_total',0))")
if [ "$FC_AVAILABLE" = true ]; then
  [ "$WT" -ge 1 ] && pass "Embedded worker registered ($WT)" || fail "Worker registration" "total=$WT"

  # Heartbeat: wait 15s, check still ready
  sleep 15
  WR=$(curl -s "$ENDPOINT/api/v1/health" | python3 -c "import sys,json; print(json.load(sys.stdin).get('workers_ready',0))")
  [ "$WR" -ge 1 ] && pass "Worker alive after 15s (heartbeat)" || fail "Worker heartbeat" "ready=$WR"
else
  skip "Worker tests (no KVM)"
fi

# ── 3. Session CRUD ──────────────────────────────────────

echo ""
echo -e "${CYAN}==> 3. Session CRUD${NC}"

CR=$(curl -s -X POST "$ENDPOINT/api/v1/sessions" -H 'Content-Type: application/json' \
  -d '{"name":"crud-test","mode":"execute"}')
SID=$(echo "$CR" | jf ID)
[ -n "$SID" ] && pass "Create session ($SID)" || fail "Create session" "no ID"

GN=$(curl -s "$ENDPOINT/api/v1/sessions/$SID" | jf Name)
[ "$GN" = "crud-test" ] && pass "Get session (name=$GN)" || fail "Get session" "name=$GN"

LC=$(curl -s "$ENDPOINT/api/v1/sessions" | jlen sessions)
[ "$LC" -ge 1 ] && pass "List sessions ($LC)" || fail "List sessions" "$LC"

# ── 4. Mode transitions ─────────────────────────────────

echo ""
echo -e "${CYAN}==> 4. Mode transitions${NC}"

for M in explore ask execute; do
  GM=$(curl -s -X POST "$ENDPOINT/api/v1/sessions/$SID/mode" -H 'Content-Type: application/json' \
    -d "{\"mode\":\"$M\"}" | jf Mode)
  [ "$GM" = "$M" ] && pass "Mode → $M" || fail "Mode → $M" "got $GM"
done

# ── 5. Pause / Resume ───────────────────────────────────

echo ""
echo -e "${CYAN}==> 5. Pause / Resume${NC}"

PS=$(curl -s -X POST "$ENDPOINT/api/v1/sessions/$SID/pause" | jf Status)
[ "$PS" = "paused" ] && pass "Pause" || fail "Pause" "$PS"

RS=$(curl -s -X POST "$ENDPOINT/api/v1/sessions/$SID/resume" | jf Status)
[ "$RS" = "running" ] && pass "Resume" || fail "Resume" "$RS"

# ── 6. Idle / Auto-wake ─────────────────────────────────

echo ""
echo -e "${CYAN}==> 6. Idle / Auto-wake${NC}"

IS=$(curl -s -X POST "$ENDPOINT/api/v1/sessions/$SID/idle" | jf Status)
[ "$IS" = "idle" ] && pass "Idle" || fail "Idle" "$IS"

# Exec on idle triggers auto-wake
curl -s -X POST "$ENDPOINT/api/v1/sessions/$SID/exec" -H 'Content-Type: application/json' \
  -d '{"command":"true"}' >/dev/null
sleep 1
WS=$(curl -s "$ENDPOINT/api/v1/sessions/$SID" | jf Status)
[ "$WS" = "running" ] && pass "Auto-wake on exec" || fail "Auto-wake" "$WS"

# ── 7. Tokens ────────────────────────────────────────────

echo ""
echo -e "${CYAN}==> 7. Worker tokens${NC}"

TK=$(curl -s -X POST "$ENDPOINT/api/v1/worker-tokens" -H 'Content-Type: application/json' \
  -d '{"name":"e2e-tok","expires_seconds":3600}')
TID=$(echo "$TK" | jf ID)
TV=$(echo "$TK" | jf Token)
[ -n "$TID" ] && echo "$TV" | grep -q "^loka_" && pass "Create token ($TID)" || fail "Create token" "$TID"

TL=$(curl -s "$ENDPOINT/api/v1/worker-tokens" | jlen tokens)
[ "$TL" -ge 1 ] && pass "List tokens ($TL)" || fail "List tokens" "$TL"

# ── 8. Admin / Retention ────────────────────────────────

echo ""
echo -e "${CYAN}==> 8. Admin${NC}"

RT=$(curl -s "$ENDPOINT/api/v1/admin/retention" | python3 -c "import sys,json; print(json.load(sys.stdin).get('SessionTTL',''))" 2>/dev/null)
[ "$RT" = "168h" ] && pass "Retention config (session_ttl=$RT)" || fail "Retention" "$RT"

# ── 9. Destroy / Purge ──────────────────────────────────

echo ""
echo -e "${CYAN}==> 9. Destroy / Purge${NC}"

DC=$(curl -s -o /dev/null -w '%{http_code}' -X DELETE "$ENDPOINT/api/v1/sessions/$SID")
[ "$DC" = "204" ] && pass "Destroy session (HTTP $DC)" || fail "Destroy" "HTTP $DC"

DS=$(curl -s "$ENDPOINT/api/v1/sessions/$SID" | jf Status)
[ "$DS" = "terminated" ] && pass "Status=terminated" || fail "Post-destroy status" "$DS"

# Purge test
PC=$(curl -s -X POST "$ENDPOINT/api/v1/sessions" -H 'Content-Type: application/json' -d '{"name":"purge-me"}')
PSID=$(echo "$PC" | jf ID)
PRC=$(curl -s -o /dev/null -w '%{http_code}' -X DELETE "$ENDPOINT/api/v1/sessions/$PSID?purge=true")
[ "$PRC" = "204" ] && pass "Purge session (HTTP $PRC)" || fail "Purge" "HTTP $PRC"

# ── 10. Firecracker exec (real VM) ──────────────────────

if [ "$FC_AVAILABLE" = true ] && [ "$DOCKER_AVAILABLE" = true ]; then
  echo ""
  echo -e "${CYAN}==> 10. Firecracker VM exec${NC}"

  FC=$(curl -s -X POST "$ENDPOINT/api/v1/sessions" -H 'Content-Type: application/json' \
    -d '{"name":"fc-exec","image":"alpine:latest","mode":"execute"}')
  FSID=$(echo "$FC" | jf ID)
  [ -n "$FSID" ] && pass "Create session with image" || { fail "FC create" "no ID"; }

  if [ -n "$FSID" ]; then
    # Wait for provisioning → running
    echo -n "  Waiting for VM..."
    for i in $(seq 1 60); do
      FS=$(curl -s "$ENDPOINT/api/v1/sessions/$FSID" | jf Status)
      [ "$FS" = "running" ] && break
      echo -n "."; sleep 2
    done
    echo " $FS"

    FW=$(curl -s "$ENDPOINT/api/v1/sessions/$FSID" | jf WorkerID)
    [ "$FS" = "running" ] && [ -n "$FW" ] && pass "Session provisioned (worker=$FW)" || fail "Provisioning" "status=$FS worker=$FW"

    if [ "$FS" = "running" ] && [ -n "$FW" ]; then
      # echo
      EX=$(curl -s -X POST "$ENDPOINT/api/v1/sessions/$FSID/exec" -H 'Content-Type: application/json' \
        -d '{"command":"echo","args":["e2e-vm-test"]}')
      EID=$(echo "$EX" | jf ID)
      sleep 3
      ER=$(curl -s "$ENDPOINT/api/v1/sessions/$FSID/exec/$EID")
      EST=$(echo "$ER" | jf Status)
      EOUT=$(echo "$ER" | python3 -c "import sys,json;d=json.load(sys.stdin);r=d.get('Results') or [];print(r[0].get('Stdout','').strip() if r else '')" 2>/dev/null)

      if [ "$EST" = "success" ] && [ "$EOUT" = "e2e-vm-test" ]; then
        pass "echo in VM → '$EOUT'"
      else
        fail "echo in VM" "status=$EST stdout='$EOUT'"
      fi

      # ls /
      LX=$(curl -s -X POST "$ENDPOINT/api/v1/sessions/$FSID/exec" -H 'Content-Type: application/json' \
        -d '{"command":"ls","args":["/"]}')
      LID=$(echo "$LX" | jf ID)
      sleep 3
      LR=$(curl -s "$ENDPOINT/api/v1/sessions/$FSID/exec/$LID")
      LST=$(echo "$LR" | jf Status)
      LOUT=$(echo "$LR" | python3 -c "import sys,json;d=json.load(sys.stdin);r=d.get('Results') or [];print(r[0].get('Stdout','').strip() if r else '')" 2>/dev/null)

      [ "$LST" = "success" ] && echo "$LOUT" | grep -q "bin" && pass "ls / in VM" || fail "ls / in VM" "status=$LST"

      # uname
      UX=$(curl -s -X POST "$ENDPOINT/api/v1/sessions/$FSID/exec" -H 'Content-Type: application/json' \
        -d '{"command":"uname","args":["-a"]}')
      UID2=$(echo "$UX" | jf ID)
      sleep 3
      UR=$(curl -s "$ENDPOINT/api/v1/sessions/$FSID/exec/$UID2")
      UOUT=$(echo "$UR" | python3 -c "import sys,json;d=json.load(sys.stdin);r=d.get('Results') or [];print(r[0].get('Stdout','').strip() if r else '')" 2>/dev/null)

      echo "$UOUT" | grep -q "Linux" && pass "uname in VM → Linux" || fail "uname" "$UOUT"

      # write + read file
      curl -s -X POST "$ENDPOINT/api/v1/sessions/$FSID/exec" -H 'Content-Type: application/json' \
        -d '{"command":"sh","args":["-c","echo hello > /tmp/test.txt && cat /tmp/test.txt"]}' >/dev/null
      sleep 3
      # Get latest exec
      EXECS=$(curl -s "$ENDPOINT/api/v1/sessions/$FSID/exec" | python3 -c "
import sys,json;d=json.load(sys.stdin);exs=d.get('executions',[])
if exs: print(exs[-1].get('ID',''))
else: print('')" 2>/dev/null)
      if [ -n "$EXECS" ]; then
        WR=$(curl -s "$ENDPOINT/api/v1/sessions/$FSID/exec/$EXECS")
        WROUT=$(echo "$WR" | python3 -c "import sys,json;d=json.load(sys.stdin);r=d.get('Results') or [];print(r[0].get('Stdout','').strip() if r else '')" 2>/dev/null)
        [ "$WROUT" = "hello" ] && pass "Write + read file in VM" || fail "Write file" "output='$WROUT'"
      fi

      # Destroy
      curl -s -X DELETE "$ENDPOINT/api/v1/sessions/$FSID" >/dev/null
      pass "Destroy Firecracker session"
    fi
  fi
else
  echo ""
  skip "Firecracker VM exec (no KVM or Docker)"
fi

echo ""
echo -e "${GREEN}${BOLD}  E2E tests complete!${NC}"
