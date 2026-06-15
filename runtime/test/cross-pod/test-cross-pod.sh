#!/bin/bash

set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
RUNTIME_DIR="$(cd "$SCRIPT_DIR/../.." && pwd)"
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
NC='\033[0m'

pass() { echo -e "${GREEN}✅ PASS${NC}: $1"; }
fail() { echo -e "${RED}❌ FAIL${NC}: $1"; exit 1; }
info() { echo -e "${YELLOW}──${NC} $1"; }
header() { echo -e "\n${CYAN}═══ $1 ═══${NC}"; }

cleanup() {
    info "Cleaning up..."
    docker rm -f pod-a pod-b 2>/dev/null || true
    docker network rm mesh-test-rpc 2>/dev/null || true
}
trap cleanup EXIT

# Cleanup
cleanup 2>/dev/null

# Build
header "Building runtime image"
cd "$RUNTIME_DIR"
docker build --build-arg LANG_RUNTIME=metacall/core:latest -t function-pod:test . 2>&1
pass "Image built"

# Network
header "Creating Docker network"
docker network create mesh-test-rpc
pass "Network created"

# Pod B (Node.js)
# Start Pod B FIRST — Pod A's rpc_loader needs to reach Pod B's /inspect on startup
header "Starting Pod B (Node.js: greet, multiply)"
docker run -d \
    --name pod-b \
    --network mesh-test-rpc \
    -p 8082:8080 \
    -v "$SCRIPT_DIR/pod-b:/app" \
    -e FUNCTION_CONFIG=/app/metacall.json \
    function-pod:test

info "Waiting for Pod B to start..."
sleep 5

# Verify Pod B is healthy
HEALTH=$(curl -sf http://localhost:8082/health 2>/dev/null || echo "FAILED")
echo "$HEALTH" | grep -q '"ok"' && pass "Pod B healthy" || fail "Pod B failed to start. Logs:\n$(docker logs pod-b 2>&1)"

# Verify Pod B /inspect
info "Checking Pod B /inspect..."
INSPECT_B=$(curl -sf http://localhost:8082/inspect)
echo "$INSPECT_B" | grep -q "greet" && pass "Pod B exposes greet()" || fail "Pod B /inspect missing greet"
echo "$INSPECT_B" | grep -q "multiply" && pass "Pod B exposes multiply()" || fail "Pod B /inspect missing multiply"

# Verify Pod B functions work
info "Testing Pod B functions directly..."
R=$(curl -sf -X POST http://localhost:8082/call/greet -H "Content-Type: application/json" -d '["test"]')
echo "  greet(\"test\") = $R"
R=$(curl -sf -X POST http://localhost:8082/call/multiply -H "Content-Type: application/json" -d '[3,4]')
echo "  multiply(3,4) = $R"
pass "Pod B functions work"

# Pod A (Python + rpc_loader)
header "Starting Pod A (Python + rpc_loader → Pod B)"

docker run -d \
    --name pod-a \
    --network mesh-test-rpc \
    -p 8081:8080 \
    -v "$SCRIPT_DIR/pod-a:/app" \
    -v "$SCRIPT_DIR/pod-a/mesh:/mesh" \
    -e FUNCTION_CONFIG=/app/metacall.json \
    -e RPC_CONFIG=/mesh/metacall-rpc.json \
    function-pod:test

info "Waiting for Pod A to start (rpc_loader discovery takes a moment)..."
sleep 8

# Check Pod A logs
header "Pod A startup logs"
docker logs pod-a 2>&1 | tail -20

# Verify Pod A is healthy
HEALTH=$(curl -sf http://localhost:8081/health 2>/dev/null || echo "FAILED")
echo "$HEALTH" | grep -q '"ok"' && pass "Pod A healthy" || fail "Pod A failed to start. Logs:\n$(docker logs pod-a 2>&1)"

# Test local function
header "Testing Pod A local function"
R=$(curl -sf -X POST http://localhost:8081/call/local_hello -H "Content-Type: application/json" -d '["world"]')
echo "  local_hello(\"world\") = $R"
echo "$R" | grep -q "Hello world" && pass "Local function works" || fail "Local function failed"

# Test cross-Pod function
header "Testing CROSS-POD call (the main test!)"
echo "  Calling test_cross_pod() in Pod A..."
echo "  This function calls greet() and multiply() which live in Pod B."
echo "  rpc_loader should route these metacall(...) calls via HTTP to Pod B."
echo ""

R=$(curl -sf -X POST http://localhost:8081/call/test_cross_pod \
    -H "Content-Type: application/json" \
    -d '[]' 2>/dev/null || echo "CROSS_POD_FAILED")

echo "  Result: $R"
echo ""

if echo "$R" | grep -q "CROSS_POD_FAILED"; then
    echo -e "${RED}Cross-Pod call failed.${NC}"
    echo ""
    echo "Pod A logs:"
    docker logs pod-a 2>&1 | tail -30
    echo ""
    echo "This might mean:"
    echo "  1. rpc_loader is not compiled into the metacall/core image"
    echo "  2. rpc_loader couldn't reach Pod B during startup"
    echo "  3. The /inspect format isn't what rpc_loader expects"
    echo ""
    echo "Check if rpc_loader is available:"
    echo "  docker exec pod-a metacall --help"
    fail "Cross-Pod call failed"
fi

echo "$R" | grep -q "greeting" && pass "Cross-Pod call returned greeting" || fail "Missing greeting in response"
echo "$R" | grep -q "product" && pass "Cross-Pod call returned product" || fail "Missing product in response"

# Summary
header "RESULTS"
echo ""
echo -e "${GREEN}════════════════════════════════════════════════════════════${NC}"
echo -e "${GREEN}  Cross-Pod rpc_loader Test: ALL TESTS PASSED              ${NC}"
echo -e "${GREEN}════════════════════════════════════════════════════════════${NC}"
echo ""
echo "Containers still running. Cleanup: docker rm -f pod-a pod-b && docker network rm mesh-test-rpc"
