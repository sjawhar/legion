#!/bin/bash
# verify-cluster.sh — NATS cluster health verification script
# Usage: ./verify-cluster.sh http://host1:8222 http://host2:8222 ...
# Checks cluster routes, stream existence, and consumer state

set -e

if [ $# -eq 0 ]; then
    echo "Usage: $0 <monitoring-url> [<monitoring-url> ...]"
    echo "Example: $0 http://localhost:8222 http://nats-2:8222"
    exit 1
fi

STREAM_NAME="ENVOY_NOTIFICATIONS"
CONSUMER_NAME="listener"
PASS_COUNT=0
FAIL_COUNT=0

echo "=== NATS Cluster Health Verification ==="
echo ""

# Check each monitoring URL
for url in "$@"; do
    echo "Checking $url..."
    
    # Check /routez — cluster routes
    if ! routes=$(curl -s "$url/routez" 2>/dev/null); then
        echo "  ✗ FAIL: Could not reach $url/routez"
        ((FAIL_COUNT++))
        continue
    fi
    
    # Extract peer count from routes response
    # routez returns JSON with "routes" array
    peer_count=$(echo "$routes" | grep -o '"routes"' | wc -l)
    if [ "$peer_count" -gt 0 ]; then
        echo "  ✓ PASS: Cluster routes reachable"
        ((PASS_COUNT++))
    else
        echo "  ✗ FAIL: No cluster routes found"
        ((FAIL_COUNT++))
    fi
    
    # Check /jsz — JetStream info and stream existence
    if ! jsz=$(curl -s "$url/jsz" 2>/dev/null); then
        echo "  ✗ FAIL: Could not reach $url/jsz"
        ((FAIL_COUNT++))
        continue
    fi
    
    # Check if ENVOY_NOTIFICATIONS stream exists
    if echo "$jsz" | grep -q "\"$STREAM_NAME\""; then
        echo "  ✓ PASS: Stream $STREAM_NAME exists"
        ((PASS_COUNT++))
    else
        echo "  ✗ FAIL: Stream $STREAM_NAME not found"
        ((FAIL_COUNT++))
    fi
    
    # Check KV buckets (envoy_interests, envoy_sessions, envoy_roles)
    for bucket in "envoy_interests" "envoy_sessions" "envoy_roles"; do
        if echo "$jsz" | grep -q "\"$bucket\""; then
            echo "  ✓ PASS: KV bucket $bucket exists"
            ((PASS_COUNT++))
        else
            echo "  ✗ FAIL: KV bucket $bucket not found"
            ((FAIL_COUNT++))
        fi
    done
    
    # Check for listener-* consumers in the jsz response
    # Extract machine ID from URL or use a generic pattern
    if echo "$jsz" | grep -q "listener-"; then
        echo "  ✓ PASS: Listener consumers found"
        ((PASS_COUNT++))
    else
        echo "  ✗ FAIL: No listener consumers found"
        ((FAIL_COUNT++))
    fi

echo ""
echo "=== Summary ==="
echo "Passed: $PASS_COUNT"
echo "Failed: $FAIL_COUNT"
echo ""

if [ $FAIL_COUNT -eq 0 ]; then
    echo "✓ All checks passed"
    exit 0
else
    echo "✗ Some checks failed"
    exit 1
fi
