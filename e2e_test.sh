#!/bin/bash
# Ephemera simple end-to-end test scenario
# Run with: sudo bash simple_test_scenario.sh
set -euo pipefail

API="http://localhost:3000"
TASK='{"prompt":"Check my current system environment. Tell me the OS version, available disk space in the current directory."}'
LOG=$(mktemp /tmp/ephemera-test-XXXXXX.log)
PASS=true

step()       { echo; echo "━━━ $* ━━━"; }
ok()         { echo "  ✓ $*"; }
fail()       { echo "  ✗ $*"; PASS=false; }
check_http() { local code=$1 expected=$2 label=$3
               [ "$code" = "$expected" ] && ok "$label (HTTP $code)" \
                                         || fail "$label (HTTP $code, expected $expected)"; }

# ── Pre-flight: clean up any leftover files from previous test runs ──
rm -f /tmp/goose-workspaces/*.ext4 2>/dev/null || true
rm -f /tmp/goose-workspaces/*.cow  2>/dev/null || true
rm -rf snapshots/snap-* 2>/dev/null || true

# ── 1. Start daemon ──────────────────────────────────────────────
step "1. Start daemon"
echo "  Working directory: $(pwd)"
echo "  Log file: $LOG"
./ephemera-daemon >>"$LOG" 2>&1 &
DAEMON_PID=$!
echo "  Daemon PID: $DAEMON_PID"

# Wait until the control plane API accepts connections
for i in $(seq 1 30); do
    curl -s -o /dev/null "$API/vms" 2>/dev/null && break
    sleep 1
done
curl -s -o /dev/null "$API/vms" && ok "Control plane API is responding" || { fail "API not responding"; exit 1; }

cleanup() {
    echo; echo "━━━ Cleanup (trap) ━━━"
    kill "$DAEMON_PID" 2>/dev/null || true
    wait "$DAEMON_PID" 2>/dev/null || true
}
trap cleanup EXIT

# ── 2. Create one VM ─────────────────────────────────────────────
step "2. Create one VM"
VM1_RESP=$(curl -s -w "\n%{http_code}" -X POST "$API/vms" \
               -H "Content-Type: application/json")
VM1_CODE=$(echo "$VM1_RESP" | tail -1)
VM1_BODY=$(echo "$VM1_RESP" | head -1)
check_http "$VM1_CODE" "201" "POST /vms"
VM1_ID=$(echo    "$VM1_BODY" | jq -r '.vm_id')
VM1_AGENT=$(echo "$VM1_BODY" | jq -r '.agent_url')
VM1_IP=$(echo    "$VM1_BODY" | jq -r '.guest_ip')
VM1_TOKEN=$(echo "$VM1_BODY" | jq -r '.agent_token')
ok "VM1: $VM1_ID  IP: $VM1_IP  Agent: $VM1_AGENT"
[ -n "$VM1_TOKEN" ] && ok "VM1 agent token received (${#VM1_TOKEN} chars)" || fail "No agent_token in response"

# ── 3. Run a task on VM1 ─────────────────────────────────────────
step "3. Run a task on VM1"
T1=$(curl -s --max-time 90 -X POST "$VM1_AGENT/tasks" \
         -H "Content-Type: application/json" \
         -H "Authorization: Bearer $VM1_TOKEN" \
         -d "$TASK")
T1_OUT=$(echo "$T1" | jq -r '.output' 2>/dev/null | grep -v '^$' | tail -3 | tr '\n' ' ')
[ -n "$T1_OUT" ] && ok "Response: $(echo "$T1_OUT" | sed 's/  */ /g')" || fail "No response"

# ── 4. Stop goose agent on VM1 ───────────────────────────────────
step "4. Stop goose agent on VM1"
S1=$(curl -s -o /dev/null -w "%{http_code}" -X POST "$VM1_AGENT/stop" \
         -H "Authorization: Bearer $VM1_TOKEN")
check_http "$S1" "200" "POST /stop"
sleep 5

# ── 5. Delete VM1 ────────────────────────────────────────────────
step "5. Delete VM1"
D1=$(curl -s -w "\n%{http_code}" -X DELETE "$API/vms/$VM1_ID")
check_http "$(echo "$D1" | tail -1)" "200" "DELETE /vms/$VM1_ID"
echo "  $(echo "$D1" | head -1 | jq -c .)"

# ── 6. Create two VMs ────────────────────────────────────────────
step "6. Create two VMs"
VM2_RESP=$(curl -s -w "\n%{http_code}" -X POST "$API/vms" \
               -H "Content-Type: application/json")
VM2_CODE=$(echo "$VM2_RESP" | tail -1)
VM2_BODY=$(echo "$VM2_RESP" | head -1)
check_http "$VM2_CODE" "201" "POST /vms (VM2)"
VM2_ID=$(echo    "$VM2_BODY" | jq -r '.vm_id')
VM2_AGENT=$(echo "$VM2_BODY" | jq -r '.agent_url')
VM2_TOKEN=$(echo "$VM2_BODY" | jq -r '.agent_token')
ok "VM2: $VM2_ID  Agent: $VM2_AGENT"

VM3_RESP=$(curl -s -w "\n%{http_code}" -X POST "$API/vms" \
               -H "Content-Type: application/json")
VM3_CODE=$(echo "$VM3_RESP" | tail -1)
VM3_BODY=$(echo "$VM3_RESP" | head -1)
check_http "$VM3_CODE" "201" "POST /vms (VM3)"
VM3_ID=$(echo    "$VM3_BODY" | jq -r '.vm_id')
VM3_AGENT=$(echo "$VM3_BODY" | jq -r '.agent_url')
VM3_TOKEN=$(echo "$VM3_BODY" | jq -r '.agent_token')
ok "VM3: $VM3_ID  Agent: $VM3_AGENT"

step "Verify VM list (should be 2)"
COUNT=$(curl -s "$API/vms" | jq 'length')
[ "$COUNT" = "2" ] && ok "VM count: $COUNT" || fail "VM count: $COUNT (expected 2)"
curl -s "$API/vms" | jq -r '.[] | "  \(.vm_id)  \(.guest_ip)"'

# ── 7. Run tasks on VM2 and VM3 in parallel ──────────────────────
step "7. Run tasks on VM2 and VM3 in parallel"
curl -s --max-time 90 -X POST "$VM2_AGENT/tasks" \
     -H "Content-Type: application/json" \
     -H "Authorization: Bearer $VM2_TOKEN" \
     -d "$TASK" >/tmp/t2.json &
PID2=$!
curl -s --max-time 90 -X POST "$VM3_AGENT/tasks" \
     -H "Content-Type: application/json" \
     -H "Authorization: Bearer $VM3_TOKEN" \
     -d "$TASK" >/tmp/t3.json &
PID3=$!
wait $PID2; wait $PID3

T2=$(jq -r '.output' /tmp/t2.json 2>/dev/null | grep -v '^$' | tail -2 | tr '\n' ' ')
T3=$(jq -r '.output' /tmp/t3.json 2>/dev/null | grep -v '^$' | tail -2 | tr '\n' ' ')
[ -n "$T2" ] && ok "VM2 response: $(echo "$T2" | sed 's/  */ /g')" || fail "No response from VM2"
[ -n "$T3" ] && ok "VM3 response: $(echo "$T3" | sed 's/  */ /g')" || fail "No response from VM3"

# ── 8. Stop goose agents on VM2 and VM3 ──────────────────────────
step "8. Stop goose agents on VM2 and VM3"
check_http "$(curl -s -o /dev/null -w "%{http_code}" -X POST "$VM2_AGENT/stop" \
              -H "Authorization: Bearer $VM2_TOKEN")" "200" "VM2 /stop"
check_http "$(curl -s -o /dev/null -w "%{http_code}" -X POST "$VM3_AGENT/stop" \
              -H "Authorization: Bearer $VM3_TOKEN")" "200" "VM3 /stop"
sleep 5

# ── 9. Delete both VMs ───────────────────────────────────────────
step "9. Delete both VMs"
check_http "$(curl -s -o /dev/null -w "%{http_code}" -X DELETE "$API/vms/$VM2_ID")" "200" "DELETE VM2"
check_http "$(curl -s -o /dev/null -w "%{http_code}" -X DELETE "$API/vms/$VM3_ID")" "200" "DELETE VM3"

step "Verify VM list is empty (should be 0)"
FCOUNT=$(curl -s "$API/vms" | jq 'length')
[ "$FCOUNT" = "0" ] && ok "VM count: $FCOUNT" || fail "VM count: $FCOUNT (expected 0)"

# ── 11. Create VM for snapshot test ──────────────────────────────
step "11. Create VM for snapshot test"
SNAPVM_RESP=$(curl -s -w "\n%{http_code}" -X POST "$API/vms" \
                  -H "Content-Type: application/json")
SNAPVM_CODE=$(echo "$SNAPVM_RESP" | tail -1)
SNAPVM_BODY=$(echo "$SNAPVM_RESP" | head -1)
check_http "$SNAPVM_CODE" "201" "POST /vms (snap-source)"
SNAPVM_ID=$(echo    "$SNAPVM_BODY" | jq -r '.vm_id')
SNAPVM_AGENT=$(echo "$SNAPVM_BODY" | jq -r '.agent_url')
SNAPVM_TOKEN=$(echo "$SNAPVM_BODY" | jq -r '.agent_token')
ok "Snap-source VM: $SNAPVM_ID  Agent: $SNAPVM_AGENT"
[ -n "$SNAPVM_TOKEN" ] && ok "Agent token received (${#SNAPVM_TOKEN} chars)" || fail "No agent_token"

# ── 12. Take snapshot (stop_after=true) ──────────────────────────
step "12. Take snapshot of snap-source VM (stop_after=true)"
SNAP_RESP=$(curl -s --max-time 300 -w "\n%{http_code}" \
                -X POST "$API/vms/$SNAPVM_ID/snapshot" \
                -H "Content-Type: application/json" \
                -d '{"stop_after": true}')
SNAP_CODE=$(echo "$SNAP_RESP" | tail -1)
SNAP_BODY=$(echo "$SNAP_RESP" | head -1)
check_http "$SNAP_CODE" "201" "POST /vms/$SNAPVM_ID/snapshot"
SNAP_ID=$(echo "$SNAP_BODY" | jq -r '.snapshot_id')
[ -n "$SNAP_ID" ] && ok "Snapshot ID: $SNAP_ID" || fail "No snapshot_id in response"
ok "Source VM: $(echo "$SNAP_BODY" | jq -r '.source_vm_id')  created: $(echo "$SNAP_BODY" | jq -r '.created_at')"

# Verify source VM was removed (stop_after=true destroys the VM after snapshotting)
GONE=$(curl -s "$API/vms" | jq --arg id "$SNAPVM_ID" '[.[] | select(.vm_id == $id)] | length')
[ "$GONE" = "0" ] && ok "Source VM removed after stop_after snapshot" \
                   || fail "Source VM still listed (expected removal)"

# ── 13. Verify snapshot list ──────────────────────────────────────
step "13. Verify snapshot list (should be 1)"
SNAP_COUNT=$(curl -s "$API/snapshots" | jq 'length')
[ "$SNAP_COUNT" = "1" ] && ok "Snapshot count: $SNAP_COUNT" || fail "Snapshot count: $SNAP_COUNT (expected 1)"
curl -s "$API/snapshots" | jq -r '.[] | "  \(.snapshot_id)  source: \(.source_vm_id)  \(.created_at)"'

# ── 14. Restore VM from snapshot ─────────────────────────────────
step "14. Restore VM from snapshot"
RESTORE_RESP=$(curl -s --max-time 120 -w "\n%{http_code}" \
                   -X POST "$API/snapshots/$SNAP_ID/restore")
RESTORE_CODE=$(echo "$RESTORE_RESP" | tail -1)
RESTORE_BODY=$(echo "$RESTORE_RESP" | head -1)
check_http "$RESTORE_CODE" "201" "POST /snapshots/$SNAP_ID/restore"
[ "$RESTORE_CODE" != "201" ] && echo "  Error body: $RESTORE_BODY"
RESTORE_VM_ID=$(echo  "$RESTORE_BODY" | jq -r '.vm_id')
RESTORE_AGENT=$(echo  "$RESTORE_BODY" | jq -r '.agent_url')
RESTORE_TOKEN=$(echo  "$RESTORE_BODY" | jq -r '.agent_token')
RESTORE_SRC=$(echo    "$RESTORE_BODY" | jq -r '.source_snapshot_id')
ok "Restored VM: $RESTORE_VM_ID  Agent: $RESTORE_AGENT"
ok "Source snapshot: $RESTORE_SRC"
[ "$RESTORE_TOKEN" = "$SNAPVM_TOKEN" ] \
    && ok "Agent token matches original ✓" \
    || fail "Agent token mismatch (got: ${RESTORE_TOKEN:0:8}...  want: ${SNAPVM_TOKEN:0:8}...)"

# ── 15. Run task on restored VM ───────────────────────────────────
step "15. Run task on restored VM"
RT=$(curl -s --max-time 90 -X POST "$RESTORE_AGENT/tasks" \
         -H "Content-Type: application/json" \
         -H "Authorization: Bearer $RESTORE_TOKEN" \
         -d "$TASK")
RT_OUT=$(echo "$RT" | jq -r '.output' 2>/dev/null | grep -v '^$' | tail -3 | tr '\n' ' ')
[ -n "$RT_OUT" ] && ok "Response: $(echo "$RT_OUT" | sed 's/  */ /g')" || fail "No response from restored VM"

# ── 16. Delete restored VM ────────────────────────────────────────
step "16. Delete restored VM"
check_http "$(curl -s -o /dev/null -w "%{http_code}" -X DELETE "$API/vms/$RESTORE_VM_ID")" \
           "200" "DELETE /vms/$RESTORE_VM_ID"

# ── 17. Delete snapshot and verify list is empty ──────────────────
step "17. Delete snapshot"
DS=$(curl -s -w "\n%{http_code}" -X DELETE "$API/snapshots/$SNAP_ID")
check_http "$(echo "$DS" | tail -1)" "200" "DELETE /snapshots/$SNAP_ID"
echo "  $(echo "$DS" | head -1 | jq -c .)"

SNAP_FINAL=$(curl -s "$API/snapshots" | jq 'length')
[ "$SNAP_FINAL" = "0" ] && ok "Snapshot count after delete: $SNAP_FINAL" \
                          || fail "Expected 0 snapshots, got $SNAP_FINAL"

# ════════════════════════════════════════════════════════════════
# Concurrent restore test: restore TWO DIFFERENT snapshots simultaneously.
#
# Same-snapshot concurrent restores are architecturally unsupported:
# the guest IP is baked into each snapshot's memory state (via the
# kernel ip= boot parameter), so two restores from the same snapshot
# would both claim the same IP → ARP conflict + network allocation 409.
#
# Different-snapshot concurrent restores DO work:
#   • Each snapshot has a distinct guest IP
#   • Bind mount gives each restore its own private disk copy
#   • Both VMs run simultaneously on different IPs
#
# This validates the bind-mount implementation (concurrent disk setup)
# and the network manager's concurrent allocation correctness.
# ════════════════════════════════════════════════════════════════

# ── 19. Create two VMs and snapshot each ─────────────────────────
step "19. Create two VMs for concurrent restore test"
CSA_RESP=$(curl -s -w "\n%{http_code}" -X POST "$API/vms" -H "Content-Type: application/json")
check_http "$(echo "$CSA_RESP" | tail -1)" "201" "POST /vms (snap-source A)"
CSA_BODY=$(echo "$CSA_RESP" | head -1)
CSA_ID=$(echo "$CSA_BODY" | jq -r '.vm_id')
CSA_TOKEN=$(echo "$CSA_BODY" | jq -r '.agent_token')
ok "Source A: $CSA_ID  IP: $(echo "$CSA_BODY" | jq -r '.guest_ip')"

CSB_RESP=$(curl -s -w "\n%{http_code}" -X POST "$API/vms" -H "Content-Type: application/json")
check_http "$(echo "$CSB_RESP" | tail -1)" "201" "POST /vms (snap-source B)"
CSB_BODY=$(echo "$CSB_RESP" | head -1)
CSB_ID=$(echo "$CSB_BODY" | jq -r '.vm_id')
CSB_TOKEN=$(echo "$CSB_BODY" | jq -r '.agent_token')
ok "Source B: $CSB_ID  IP: $(echo "$CSB_BODY" | jq -r '.guest_ip')"

# ── 20. Snapshot both VMs (stop_after=true) ──────────────────────
step "20. Snapshot both VMs (stop_after=true)"
SNAPA_RESP=$(curl -s --max-time 300 -w "\n%{http_code}" \
                 -X POST "$API/vms/$CSA_ID/snapshot" \
                 -H "Content-Type: application/json" -d '{"stop_after":true}')
check_http "$(echo "$SNAPA_RESP" | tail -1)" "201" "Snapshot A"
SNAPA_ID=$(echo "$SNAPA_RESP" | head -1 | jq -r '.snapshot_id')
ok "Snapshot A: $SNAPA_ID"

SNAPB_RESP=$(curl -s --max-time 300 -w "\n%{http_code}" \
                 -X POST "$API/vms/$CSB_ID/snapshot" \
                 -H "Content-Type: application/json" -d '{"stop_after":true}')
check_http "$(echo "$SNAPB_RESP" | tail -1)" "201" "Snapshot B"
SNAPB_ID=$(echo "$SNAPB_RESP" | head -1 | jq -r '.snapshot_id')
ok "Snapshot B: $SNAPB_ID"

SNAP2_COUNT=$(curl -s "$API/snapshots" | jq 'length')
[ "$SNAP2_COUNT" = "2" ] && ok "Snapshot count: $SNAP2_COUNT" \
                          || fail "Expected 2 snapshots, got $SNAP2_COUNT"

# ── 21. Restore both snapshots concurrently ──────────────────────
step "21. Restore two different snapshots concurrently"
curl -s --max-time 300 -w "\n%{http_code}" \
     -X POST "$API/snapshots/$SNAPA_ID/restore" >/tmp/cra.txt &
PID_CRA=$!
curl -s --max-time 300 -w "\n%{http_code}" \
     -X POST "$API/snapshots/$SNAPB_ID/restore" >/tmp/crb.txt &
PID_CRB=$!
wait $PID_CRA; wait $PID_CRB

CRA_CODE=$(tail -1 /tmp/cra.txt); CRA_BODY=$(head -1 /tmp/cra.txt)
CRB_CODE=$(tail -1 /tmp/crb.txt); CRB_BODY=$(head -1 /tmp/crb.txt)
check_http "$CRA_CODE" "201" "Restore A from $SNAPA_ID"
check_http "$CRB_CODE" "201" "Restore B from $SNAPB_ID"

CRA_VM_ID=$(echo "$CRA_BODY" | jq -r '.vm_id')
CRA_AGENT=$(echo "$CRA_BODY" | jq -r '.agent_url')
CRA_TOKEN=$(echo "$CRA_BODY" | jq -r '.agent_token')
CRB_VM_ID=$(echo "$CRB_BODY" | jq -r '.vm_id')
CRB_AGENT=$(echo "$CRB_BODY" | jq -r '.agent_url')
CRB_TOKEN=$(echo "$CRB_BODY" | jq -r '.agent_token')
ok "Restore A: $CRA_VM_ID  Agent: $CRA_AGENT"
ok "Restore B: $CRB_VM_ID  Agent: $CRB_AGENT"

[ "$CRA_VM_ID" != "$CRB_VM_ID" ] \
    && ok "Restored VMs have distinct vm_ids ✓" \
    || fail "Restored VMs share the same vm_id"
[ "$CRA_TOKEN" = "$CSA_TOKEN" ] \
    && ok "Restore A agent token matches source A ✓" \
    || fail "Restore A token mismatch"
[ "$CRB_TOKEN" = "$CSB_TOKEN" ] \
    && ok "Restore B agent token matches source B ✓" \
    || fail "Restore B token mismatch"

# ── 22. Verify both restored VMs are running simultaneously ──────
step "22. Verify both restored VMs running at the same time"
CONCURRENT_COUNT=$(curl -s "$API/vms" | jq 'length')
[ "$CONCURRENT_COUNT" = "2" ] \
    && ok "VM count: $CONCURRENT_COUNT (both restores alive simultaneously ✓)" \
    || fail "VM count: $CONCURRENT_COUNT (expected 2)"
curl -s "$API/vms" | jq -r '.[] | "  \(.vm_id)  \(.guest_ip)"'

# ── 23. Health-check both agents ─────────────────────────────────
step "23. Verify both restored agents respond"
HA=$(curl -s -o /dev/null -w "%{http_code}" "$CRA_AGENT/health")
check_http "$HA" "200" "Restore A /health"
HB=$(curl -s -o /dev/null -w "%{http_code}" "$CRB_AGENT/health")
check_http "$HB" "200" "Restore B /health"

# ── 24. Cleanup ───────────────────────────────────────────────────
step "24. Cleanup concurrent restore test"
check_http "$(curl -s -o /dev/null -w "%{http_code}" -X POST "$CRA_AGENT/stop" \
              -H "Authorization: Bearer $CRA_TOKEN")" "200" "Restore A /stop"
check_http "$(curl -s -o /dev/null -w "%{http_code}" -X POST "$CRB_AGENT/stop" \
              -H "Authorization: Bearer $CRB_TOKEN")" "200" "Restore B /stop"
sleep 3

check_http "$(curl -s -o /dev/null -w "%{http_code}" -X DELETE "$API/vms/$CRA_VM_ID")" \
           "200" "DELETE restore A"
check_http "$(curl -s -o /dev/null -w "%{http_code}" -X DELETE "$API/vms/$CRB_VM_ID")" \
           "200" "DELETE restore B"

for SID in $SNAPA_ID $SNAPB_ID; do
    DS=$(curl -s -w "\n%{http_code}" -X DELETE "$API/snapshots/$SID")
    check_http "$(echo "$DS" | tail -1)" "200" "DELETE /snapshots/$SID"
done

FINAL_VM_COUNT=$(curl -s "$API/vms" | jq 'length')
[ "$FINAL_VM_COUNT" = "0" ] && ok "VM count after cleanup: $FINAL_VM_COUNT" \
                             || fail "Expected 0 VMs, got $FINAL_VM_COUNT"
FINAL_SNAP_COUNT=$(curl -s "$API/snapshots" | jq 'length')
[ "$FINAL_SNAP_COUNT" = "0" ] && ok "Snapshot count after cleanup: $FINAL_SNAP_COUNT" \
                               || fail "Expected 0 snapshots, got $FINAL_SNAP_COUNT"

# ════════════════════════════════════════════════════════════════
# Diff Snapshot test: validate multi-checkpoint size optimization.
#
# Flow:
#   VM → snapshot #1 (auto → Full, 2 GB)
#     → snapshot #2 (auto → Diff, dirty pages only, << 2 GB)
#     → restore from Diff → verify agent responds
#     → try deleting Full while Diff exists → expect 409
#     → delete Diff first, then Full
# ════════════════════════════════════════════════════════════════

# ── 26. Create VM for diff snapshot test ─────────────────────────
step "26. Create VM for diff snapshot test"
DSNAP_VM_RESP=$(curl -s -w "\n%{http_code}" -X POST "$API/vms" -H "Content-Type: application/json")
check_http "$(echo "$DSNAP_VM_RESP" | tail -1)" "201" "POST /vms (diff-test source)"
DSNAP_VM_BODY=$(echo "$DSNAP_VM_RESP" | head -1)
DSNAP_VM_ID=$(echo    "$DSNAP_VM_BODY" | jq -r '.vm_id')
DSNAP_VM_TOKEN=$(echo "$DSNAP_VM_BODY" | jq -r '.agent_token')
ok "Diff-test source VM: $DSNAP_VM_ID"

# ── 27. Take snapshot #1 (auto → Full) ───────────────────────────
step "27. Take snapshot #1 (auto-detect → Full)"
FULL_SNAP_RESP=$(curl -s --max-time 300 -w "\n%{http_code}" \
                     -X POST "$API/vms/$DSNAP_VM_ID/snapshot" \
                     -H "Content-Type: application/json")
check_http "$(echo "$FULL_SNAP_RESP" | tail -1)" "201" "POST /vms/$DSNAP_VM_ID/snapshot (#1)"
FULL_SNAP_BODY=$(echo "$FULL_SNAP_RESP" | head -1)
FULL_SNAP_ID=$(echo "$FULL_SNAP_BODY"   | jq -r '.snapshot_id')
FULL_SNAP_TYPE=$(echo "$FULL_SNAP_BODY" | jq -r '.snapshot_type')
ok "Snapshot #1: $FULL_SNAP_ID"
[ "$FULL_SNAP_TYPE" = "full" ] \
    && ok "snapshot_type = full ✓" \
    || fail "Expected snapshot_type=full, got: $FULL_SNAP_TYPE"
[ "$(echo "$FULL_SNAP_BODY" | jq -r '.base_snapshot_id')" = "null" ] \
    && ok "base_snapshot_id is absent (full snapshot has no base) ✓" \
    || fail "Full snapshot should not have base_snapshot_id"

# ── 28. Take snapshot #2 (auto → Diff) ───────────────────────────
step "28. Take snapshot #2 (auto-detect → Diff)"
DIFF_SNAP_RESP=$(curl -s --max-time 300 -w "\n%{http_code}" \
                     -X POST "$API/vms/$DSNAP_VM_ID/snapshot" \
                     -H "Content-Type: application/json")
check_http "$(echo "$DIFF_SNAP_RESP" | tail -1)" "201" "POST /vms/$DSNAP_VM_ID/snapshot (#2)"
DIFF_SNAP_BODY=$(echo "$DIFF_SNAP_RESP" | head -1)
DIFF_SNAP_ID=$(echo "$DIFF_SNAP_BODY"       | jq -r '.snapshot_id')
DIFF_SNAP_TYPE=$(echo "$DIFF_SNAP_BODY"     | jq -r '.snapshot_type')
DIFF_BASE_ID=$(echo "$DIFF_SNAP_BODY"       | jq -r '.base_snapshot_id')
ok "Snapshot #2: $DIFF_SNAP_ID"
[ "$DIFF_SNAP_TYPE" = "diff" ] \
    && ok "snapshot_type = diff ✓" \
    || fail "Expected snapshot_type=diff, got: $DIFF_SNAP_TYPE"
[ "$DIFF_BASE_ID" = "$FULL_SNAP_ID" ] \
    && ok "base_snapshot_id matches Full snapshot ✓" \
    || fail "base_snapshot_id mismatch (got: $DIFF_BASE_ID, want: $FULL_SNAP_ID)"

# ── 29. Compare memory.bin disk usage (sparse-aware) ─────────────
# Firecracker writes Diff memory files as sparse files: only dirty pages
# consume actual disk blocks; clean pages are holes.
# stat -c%s reports the apparent (logical) size, which equals 2 GB for
# both Full and Diff. stat -c%b reports the number of 512-byte blocks
# actually allocated on disk — this is the correct metric for sparse files.
step "29. Verify Diff memory.bin uses fewer disk blocks than Full (sparse-aware)"
FULL_MEM_PATH="$PWD/snapshots/$FULL_SNAP_ID/memory.bin"
DIFF_MEM_PATH="$PWD/snapshots/$DIFF_SNAP_ID/memory.bin"
FULL_MEM_BLOCKS=$(stat -c%b "$FULL_MEM_PATH" 2>/dev/null || echo 0)
DIFF_MEM_BLOCKS=$(stat -c%b "$DIFF_MEM_PATH" 2>/dev/null || echo 0)
if [ "$FULL_MEM_BLOCKS" -gt 0 ] && [ "$DIFF_MEM_BLOCKS" -gt 0 ]; then
    FULL_MB=$(( FULL_MEM_BLOCKS * 512 / 1048576 ))
    DIFF_MB=$(( DIFF_MEM_BLOCKS * 512 / 1048576 ))
    ok "Full memory.bin disk usage: ~${FULL_MB} MB  |  Diff memory.bin disk usage: ~${DIFF_MB} MB"
    ok "(Apparent size is always 2048 MB for both; sparse holes are excluded from block count)"
    [ "$DIFF_MEM_BLOCKS" -lt "$FULL_MEM_BLOCKS" ] \
        && ok "Diff allocates fewer blocks than Full ✓" \
        || fail "Diff (~${DIFF_MB} MB blocks) is not smaller than Full (~${FULL_MB} MB blocks)"
else
    fail "Could not stat memory.bin files (blocks: full=$FULL_MEM_BLOCKS diff=$DIFF_MEM_BLOCKS)"
fi

# ── 30. Stop source VM (required for diff restore) ────────────────
step "30. Stop source VM before restoring diff snapshot"
check_http "$(curl -s -o /dev/null -w "%{http_code}" -X POST \
              "$(echo "$DSNAP_VM_BODY" | jq -r '.agent_url')/stop" \
              -H "Authorization: Bearer $DSNAP_VM_TOKEN")" "200" "source VM /stop"
sleep 2
check_http "$(curl -s -o /dev/null -w "%{http_code}" -X DELETE "$API/vms/$DSNAP_VM_ID")" \
           "200" "DELETE source VM"

# ── 31. Restore from Diff snapshot ───────────────────────────────
step "31. Restore from Diff snapshot"
DIFF_RESTORE_RESP=$(curl -s --max-time 300 -w "\n%{http_code}" \
                        -X POST "$API/snapshots/$DIFF_SNAP_ID/restore")
DIFF_RESTORE_CODE=$(echo "$DIFF_RESTORE_RESP" | tail -1)
DIFF_RESTORE_BODY=$(echo "$DIFF_RESTORE_RESP" | head -1)
check_http "$DIFF_RESTORE_CODE" "201" "POST /snapshots/$DIFF_SNAP_ID/restore"
[ "$DIFF_RESTORE_CODE" != "201" ] && echo "  Error: $DIFF_RESTORE_BODY"
DIFF_RESTORE_VM_ID=$(echo  "$DIFF_RESTORE_BODY" | jq -r '.vm_id')
DIFF_RESTORE_AGENT=$(echo  "$DIFF_RESTORE_BODY" | jq -r '.agent_url')
DIFF_RESTORE_TOKEN=$(echo  "$DIFF_RESTORE_BODY" | jq -r '.agent_token')
ok "Restored from Diff: $DIFF_RESTORE_VM_ID  Agent: $DIFF_RESTORE_AGENT"
[ "$DIFF_RESTORE_TOKEN" = "$DSNAP_VM_TOKEN" ] \
    && ok "Agent token matches original ✓" \
    || fail "Token mismatch (got: ${DIFF_RESTORE_TOKEN:0:8}...)"

# ── 32. Verify restored VM responds ──────────────────────────────
step "32. Verify Diff-restored agent responds"
check_http "$(curl -s -o /dev/null -w "%{http_code}" "$DIFF_RESTORE_AGENT/health")" \
           "200" "Diff-restored VM /health"

# ── 33. Try deleting Full snapshot while Diff references it → 409 ─
step "33. Attempt to delete Full snapshot (should fail — Diff depends on it)"
FULL_DEL_CODE=$(curl -s -o /dev/null -w "%{http_code}" -X DELETE "$API/snapshots/$FULL_SNAP_ID")
[ "$FULL_DEL_CODE" = "409" ] \
    && ok "DELETE Full returned 409 Conflict (Diff dependency correctly blocked) ✓" \
    || fail "Expected 409 Conflict, got HTTP $FULL_DEL_CODE"

# ── 34. Cleanup diff snapshot test ───────────────────────────────
step "34. Cleanup diff snapshot test"
check_http "$(curl -s -o /dev/null -w "%{http_code}" -X POST "$DIFF_RESTORE_AGENT/stop" \
              -H "Authorization: Bearer $DIFF_RESTORE_TOKEN")" "200" "Diff-restored VM /stop"
sleep 2
check_http "$(curl -s -o /dev/null -w "%{http_code}" -X DELETE "$API/vms/$DIFF_RESTORE_VM_ID")" \
           "200" "DELETE Diff-restored VM"

# Delete Diff first (removes dependency), then Full
check_http "$(curl -s -o /dev/null -w "%{http_code}" -X DELETE "$API/snapshots/$DIFF_SNAP_ID")" \
           "200" "DELETE Diff snapshot"
check_http "$(curl -s -o /dev/null -w "%{http_code}" -X DELETE "$API/snapshots/$FULL_SNAP_ID")" \
           "200" "DELETE Full snapshot (now unblocked)"

DIFF_FINAL_SNAP=$(curl -s "$API/snapshots" | jq 'length')
[ "$DIFF_FINAL_SNAP" = "0" ] && ok "Snapshot count after cleanup: $DIFF_FINAL_SNAP" \
                              || fail "Expected 0 snapshots, got $DIFF_FINAL_SNAP"

# ════════════════════════════════════════════════════════════════
# COW Rootfs dm-snapshot test: validate block-level copy-on-write.
#
# On restore, the daemon creates a dm-snapshot COW device backed by the
# snapshot's rootfs.ext4 (read-only base), with per-VM writes going to
# a sparse exception store (.cow file). The actual disk usage starts at
# near-zero and grows only as the guest writes to the rootfs.
#
# Flow:
#   VM → snapshot (stop_after=true)
#     → restore → verify /dev/mapper/cow-* device active
#     → verify exception store has near-zero initial allocation
#     → verify agent health
#     → delete VM → verify dm device, loop device, and .cow file all removed
# ════════════════════════════════════════════════════════════════

# ── 36. Create VM for COW rootfs test ───────────────────────────
step "36. Create VM for COW rootfs test"
COW_VM_RESP=$(curl -s -w "\n%{http_code}" -X POST "$API/vms" -H "Content-Type: application/json")
check_http "$(echo "$COW_VM_RESP" | tail -1)" "201" "POST /vms (cow-test source)"
COW_VM_BODY=$(echo "$COW_VM_RESP" | head -1)
COW_VM_ID=$(echo    "$COW_VM_BODY" | jq -r '.vm_id')
COW_VM_TOKEN=$(echo "$COW_VM_BODY" | jq -r '.agent_token')
ok "COW-test source VM: $COW_VM_ID"

# ── 37. Take snapshot (stop_after=true) ─────────────────────────
step "37. Take snapshot of COW-test VM (stop_after=true)"
COW_SNAP_RESP=$(curl -s --max-time 300 -w "\n%{http_code}" \
                    -X POST "$API/vms/$COW_VM_ID/snapshot" \
                    -H "Content-Type: application/json" \
                    -d '{"stop_after": true}')
check_http "$(echo "$COW_SNAP_RESP" | tail -1)" "201" "POST /vms/$COW_VM_ID/snapshot"
COW_SNAP_ID=$(echo "$COW_SNAP_RESP" | head -1 | jq -r '.snapshot_id')
ok "Snapshot: $COW_SNAP_ID"

# ── 38. Restore from snapshot ────────────────────────────────────
step "38. Restore from snapshot (COW path)"
COW_RESTORE_RESP=$(curl -s --max-time 120 -w "\n%{http_code}" \
                       -X POST "$API/snapshots/$COW_SNAP_ID/restore")
COW_RESTORE_CODE=$(echo "$COW_RESTORE_RESP" | tail -1)
COW_RESTORE_BODY=$(echo "$COW_RESTORE_RESP" | head -1)
check_http "$COW_RESTORE_CODE" "201" "POST /snapshots/$COW_SNAP_ID/restore"
[ "$COW_RESTORE_CODE" != "201" ] && echo "  Error: $COW_RESTORE_BODY"
COW_RESTORE_VM_ID=$(echo  "$COW_RESTORE_BODY" | jq -r '.vm_id')
COW_RESTORE_AGENT=$(echo  "$COW_RESTORE_BODY" | jq -r '.agent_url')
COW_RESTORE_TOKEN=$(echo  "$COW_RESTORE_BODY" | jq -r '.agent_token')
ok "Restored VM: $COW_RESTORE_VM_ID  Agent: $COW_RESTORE_AGENT"
[ "$COW_RESTORE_TOKEN" = "$COW_VM_TOKEN" ] \
    && ok "Agent token matches original ✓" \
    || fail "Agent token mismatch (got: ${COW_RESTORE_TOKEN:0:8}...  want: ${COW_VM_TOKEN:0:8}...)"

# ── 39. Verify dm-snapshot device is active ──────────────────────
step "39. Verify dm-snapshot COW device is active"
COW_DEV_COUNT=$(dmsetup ls 2>/dev/null | grep -c "^cow-" || true)
[ "${COW_DEV_COUNT:-0}" -ge "1" ] \
    && ok "dm-snapshot device active (count: $COW_DEV_COUNT) ✓" \
    || fail "No dm-snapshot device found in 'dmsetup ls' (expected at least one cow-* device)"
dmsetup ls 2>/dev/null | grep "^cow-" | sed 's/^/  dm device: /' || true

# ── 40. Verify exception store has minimal initial allocation ────
step "40. Verify exception store initial disk usage is minimal"
COW_FILE=$(ls /tmp/goose-workspaces/*.cow 2>/dev/null | head -1 || true)
if [ -n "$COW_FILE" ]; then
    COW_BLOCKS=$(stat -c%b "$COW_FILE")
    COW_ACTUAL_MB=$(( COW_BLOCKS * 512 / 1048576 ))
    ok "Exception store: $(basename "$COW_FILE")  (actual blocks: $COW_BLOCKS × 512 B = ~${COW_ACTUAL_MB} MB)"
    ok "(Apparent size is 8 GB sparse; only written blocks consume real disk space)"
    [ "$COW_ACTUAL_MB" -lt "100" ] \
        && ok "Exception store initial allocation is minimal (< 100 MB actual) ✓" \
        || fail "Exception store unexpectedly large at start: ${COW_ACTUAL_MB} MB (expected < 100 MB)"
else
    fail "No .cow exception store found in /tmp/goose-workspaces/"
fi

# ── 41. Verify restored agent responds ──────────────────────────
step "41. Verify COW-restored agent responds"
check_http "$(curl -s -o /dev/null -w "%{http_code}" "$COW_RESTORE_AGENT/health")" \
           "200" "COW-restored VM /health"

# ── 42. Delete restored VM and verify full COW cleanup ──────────
step "42. Delete COW-restored VM and verify kernel resource cleanup"
check_http "$(curl -s -o /dev/null -w "%{http_code}" -X POST "$COW_RESTORE_AGENT/stop" \
              -H "Authorization: Bearer $COW_RESTORE_TOKEN")" "200" "COW-restored VM /stop"
sleep 2
check_http "$(curl -s -o /dev/null -w "%{http_code}" -X DELETE "$API/vms/$COW_RESTORE_VM_ID")" \
           "200" "DELETE COW-restored VM"

# dm device must be gone
COW_DEV_AFTER=$(dmsetup ls 2>/dev/null | grep -c "^cow-" || true)
[ "${COW_DEV_AFTER:-0}" = "0" ] \
    && ok "dm-snapshot device removed after VM delete ✓" \
    || fail "dm-snapshot device(s) still present after VM delete (count: $COW_DEV_AFTER)"

# exception store must be removed
# find always exits 0 (safe under pipefail); tr -d ' ' strips wc's leading spaces
COW_FILE_AFTER=$(find /tmp/goose-workspaces -maxdepth 1 -name "*.cow" 2>/dev/null | wc -l | tr -d ' ')
[ "$COW_FILE_AFTER" = "0" ] \
    && ok "Exception store (.cow) removed after VM delete ✓" \
    || fail "Exception store file(s) still present after VM delete: $COW_FILE_AFTER"

# ── 43. Delete COW snapshot ──────────────────────────────────────
step "43. Delete COW snapshot"
check_http "$(curl -s -o /dev/null -w "%{http_code}" -X DELETE "$API/snapshots/$COW_SNAP_ID")" \
           "200" "DELETE /snapshots/$COW_SNAP_ID"
COW_FINAL_SNAP=$(curl -s "$API/snapshots" | jq 'length')
[ "$COW_FINAL_SNAP" = "0" ] && ok "Snapshot count after cleanup: $COW_FINAL_SNAP" \
                             || fail "Expected 0 snapshots, got $COW_FINAL_SNAP"

# ════════════════════════════════════════════════════════════════
# Agent Proxy test: verify control plane proxy endpoints.
#
# External clients can reach VM agents entirely through the control
# plane URL using /vms/{vm_id}/health, /vms/{vm_id}/tasks,
# /vms/{vm_id}/stop — no direct access to the private 10.0.1.x
# subnet required.
#
# When EPHEMERA_PUBLIC_URL is set, agent_url in VM responses
# points to the proxy path instead of the private IP.
# ════════════════════════════════════════════════════════════════

# ── 45. Create VM for agent proxy test ──────────────────────────
step "45. Create VM for agent proxy endpoint test"
PROXY_VM_RESP=$(curl -s -w "\n%{http_code}" -X POST "$API/vms" -H "Content-Type: application/json")
check_http "$(echo "$PROXY_VM_RESP" | tail -1)" "201" "POST /vms (proxy-test)"
PROXY_VM_BODY=$(echo "$PROXY_VM_RESP" | head -1)
PROXY_VM_ID=$(echo "$PROXY_VM_BODY" | jq -r '.vm_id')
PROXY_VM_TOKEN=$(echo "$PROXY_VM_BODY" | jq -r '.agent_token')
ok "Proxy-test VM: $PROXY_VM_ID"

# ── 46. Test proxy: GET /vms/{vm_id}/health ──────────────────────
step "46. Test proxy: GET /vms/\$PROXY_VM_ID/health"
check_http "$(curl -s -o /dev/null -w "%{http_code}" "$API/vms/$PROXY_VM_ID/health")" \
           "200" "GET /vms/$PROXY_VM_ID/health (proxy)"

# ── 47. Test proxy: POST /vms/{vm_id}/stop + cleanup ────────────
step "47. Test proxy: POST /vms/\$PROXY_VM_ID/stop"
check_http "$(curl -s -o /dev/null -w "%{http_code}" -X POST "$API/vms/$PROXY_VM_ID/stop")" \
           "200" "POST /vms/$PROXY_VM_ID/stop (proxy)"
sleep 2
check_http "$(curl -s -o /dev/null -w "%{http_code}" -X DELETE "$API/vms/$PROXY_VM_ID")" \
           "200" "DELETE proxy-test VM"

# ── 48. Restart daemon with EPHEMERA_PUBLIC_URL ──────────────────
step "48. Restart daemon with EPHEMERA_PUBLIC_URL=http://localhost:3000"
kill "$DAEMON_PID" 2>/dev/null; wait "$DAEMON_PID" 2>/dev/null || true
EPHEMERA_PUBLIC_URL=http://localhost:3000 ./ephemera-daemon >>"$LOG" 2>&1 &
DAEMON_PID=$!
for i in $(seq 1 30); do
    curl -s -o /dev/null "$API/vms" 2>/dev/null && break
    sleep 1
done
ok "Daemon restarted with EPHEMERA_PUBLIC_URL"

PUBVM_RESP=$(curl -s -w "\n%{http_code}" -X POST "$API/vms" -H "Content-Type: application/json")
check_http "$(echo "$PUBVM_RESP" | tail -1)" "201" "POST /vms (EPHEMERA_PUBLIC_URL test)"
PUBVM_BODY=$(echo "$PUBVM_RESP" | head -1)
PUBVM_ID=$(echo    "$PUBVM_BODY" | jq -r '.vm_id')
PUBVM_AGENT_URL=$(echo "$PUBVM_BODY" | jq -r '.agent_url')
ok "VM: $PUBVM_ID  agent_url: $PUBVM_AGENT_URL"

EXPECTED_AGENT_URL="http://localhost:3000/vms/$PUBVM_ID"
[ "$PUBVM_AGENT_URL" = "$EXPECTED_AGENT_URL" ] \
    && ok "agent_url is proxy path ✓ ($PUBVM_AGENT_URL)" \
    || fail "agent_url mismatch (got: $PUBVM_AGENT_URL, want: $EXPECTED_AGENT_URL)"

# ── 49. Test agent_url-based proxy access ────────────────────────
step "49. Verify proxy access via agent_url"
check_http "$(curl -s -o /dev/null -w "%{http_code}" "$PUBVM_AGENT_URL/health")" \
           "200" "\$agent_url/health (via proxy)"
check_http "$(curl -s -o /dev/null -w "%{http_code}" -X POST "$PUBVM_AGENT_URL/stop")" \
           "200" "\$agent_url/stop (via proxy)"
sleep 2
check_http "$(curl -s -o /dev/null -w "%{http_code}" -X DELETE "$API/vms/$PUBVM_ID")" \
           "200" "DELETE EPHEMERA_PUBLIC_URL test VM"

# ── 50. Shut down daemon ──────────────────────────────────────────
step "50. Shut down daemon"
kill "$DAEMON_PID" 2>/dev/null; wait "$DAEMON_PID" 2>/dev/null || true

trap - EXIT
ok "Daemon stopped"

# ── Result ───────────────────────────────────────────────────────
echo
if $PASS; then
    echo "══════════════════════════════════"
    echo "  All test steps passed ✓"
    echo "══════════════════════════════════"
else
    echo "══════════════════════════════════"
    echo "  Some steps failed ✗  (log: $LOG)"
    echo "══════════════════════════════════"
    exit 1
fi
