# E2E Envoy Auto-Subscription Delivery Verification Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking. Load `envoy` and `systematic-debugging` skills before beginning.

**Goal:** Verify the full Envoy auto-subscription delivery path (daemon subscribes → external event → Envoy delivers → OpenCode session receives with agent intact) via live smoke tests on the shared serve, capturing evidence artifacts for 5 acceptance criteria plus a hard-blocker gate.

**Architecture:** This is a verification-only task — no code changes, no commits. The implementer runs shell commands against live infrastructure (daemon HTTP API, Envoy HTTP API, OpenCode serve API, GitHub CLI) and captures timestamped evidence. One consolidated report is posted to issue #210 at the end.

**Tech Stack:** bash/curl (API calls), gh CLI (GitHub comments/PRs), jq (JSON parsing), OpenCode serve API (session inspection)

---

## Preamble

### Shell Variables

All tasks use these variables. Task 1 discovers and exports them. Subsequent tasks reference them directly.

```bash
export DAEMON_PORT=       # discovered from LEGION_DAEMON_PORT or default 13370
export ENVOY_URL=         # discovered from ENVOY_URL or default http://127.0.0.1:9020
export SERVE_PORT=        # discovered from daemon worker entries or default 13381
export OWNER=sjawhar
export REPO=legion
export TEST_ISSUE=210
```

### Worker Tracking

Every worker dispatched by this plan is tracked for cleanup. Store IDs as they're created:

```bash
CREATED_WORKERS=()   # array of worker IDs created by this plan
```

### Transcript Poll Helper

Use this pattern for all delivery checks (replaces ad-hoc sleep+check):

```bash
# Poll session transcript for a tag, up to 60 seconds
poll_transcript() {
  local session_id="$1" tag="$2" max_wait=60 interval=10 elapsed=0
  while [ $elapsed -lt $max_wait ]; do
    sleep $interval
    elapsed=$((elapsed + interval))
    MATCH=$(curl -fsS "http://127.0.0.1:$SERVE_PORT/session/$session_id/messages" \
      | jq -r "[.[] | select(.parts[0].text // \"\" | test(\"$tag\"))] | last // empty | .parts[0].text // \"\"" 2>/dev/null)
    if [ -n "$MATCH" ]; then
      echo "FOUND after ${elapsed}s: $(echo "$MATCH" | head -c 500)"
      return 0
    fi
    echo "Poll ${elapsed}s: not found yet..."
  done
  echo "NOT FOUND after ${max_wait}s"
  return 1
}
```

### Metis Pre-Analysis Findings

1. **Fire-and-forget subscription false positives:** Always confirm subscriptions via `GET $ENVOY_URL/v1/interests/{sessionId}` before triggering events.
2. **Session bootstrap delay:** After dispatching a worker, wait ≥5s before checking session status (see `docs/solutions/daemon/prompt-delivery-bootstrap-delay.md`).
3. **Runtime evidence only:** Every PASS verdict must cite concrete API responses or transcript excerpts. Reasoning from code is not evidence.
4. **Cross-machine routing:** Verify machine_id alignment — Envoy only delivers to sessions on the matching machine.

---

### Task 1: Environment Discovery & Prerequisites — Independent

**Files:** None (verification only)

- [ ] **Step 1: Discover and export daemon port**

```bash
export DAEMON_PORT="${LEGION_DAEMON_PORT:-13370}"
HEALTH=$(curl -fsS "http://127.0.0.1:$DAEMON_PORT/health")
echo "$HEALTH" | jq .
```

Expected: `{"status":"ok","workerCount":...,"runtime":"opencode",...}`. If connection refused → STOP, report daemon not running.

- [ ] **Step 2: Discover and export Envoy URL**

```bash
export ENVOY_URL="${ENVOY_URL:-http://127.0.0.1:9020}"
curl -fsS "$ENVOY_URL/v1/interests/" | jq 'length'
```

Expected: Returns a number (array length). Connection error → STOP, Envoy unreachable.

- [ ] **Step 3: Discover shared serve port**

```bash
# Get port from an existing worker entry, fallback to 13381
WORKER_PORT=$(curl -fsS "http://127.0.0.1:$DAEMON_PORT/workers" | jq -r '.[0].port // empty' 2>/dev/null)
export SERVE_PORT="${WORKER_PORT:-13381}"
curl -fsS "http://127.0.0.1:$SERVE_PORT/global/health" | jq .
```

Expected: `{"healthy":true}`. Unreachable → STOP, shared serve not running.

- [ ] **Step 4: Verify GitHub CLI authentication**

```bash
gh issue view 210 --json state -R sjawhar/legion | jq -r .state
```

Expected: `OPEN`. Failure → STOP, gh CLI not authenticated.

- [ ] **Step 5: Record environment snapshot**

```bash
ENV_SNAPSHOT="Daemon: http://127.0.0.1:$DAEMON_PORT | Envoy: $ENVOY_URL | Serve: http://127.0.0.1:$SERVE_PORT | Time: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
echo "$ENV_SNAPSHOT"
```

Store `$ENV_SNAPSHOT` — it goes into the final report.

---

### Task 2: Step 0 — Agent Preservation Gate (HARD BLOCKER) — Depends on: Task 1

If agent identity is not preserved through `prompt_async` without an `agent` field, ALL downstream ACs are uninterpretable.

**Files:** None

- [ ] **Step 1: Find or create a worker session**

```bash
# Find a running worker
GATE_WORKER=$(curl -fsS "http://127.0.0.1:$DAEMON_PORT/workers" \
  | jq -r '[.[] | select(.status == "running")][0] // empty')

if [ -z "$GATE_WORKER" ]; then
  # No running workers — dispatch one
  GATE_WORKER=$(curl -fsS -X POST "http://127.0.0.1:$DAEMON_PORT/workers" \
    -H "Content-Type: application/json" \
    -d '{
      "issueId": "sjawhar-legion-210",
      "mode": "plan",
      "repo": "sjawhar/legion",
      "issueNumber": 210,
      "prompt": "You are a test session for E2E verification. Wait for further instructions."
    }')
  CREATED_WORKERS+=("sjawhar-legion-210-plan")
  sleep 5  # bootstrap delay
fi

export GATE_WORKER_ID=$(echo "$GATE_WORKER" | jq -r '.id')
export GATE_SESSION_ID=$(echo "$GATE_WORKER" | jq -r '.sessionId')
echo "Worker: $GATE_WORKER_ID  Session: $GATE_SESSION_ID"
```

- [ ] **Step 2: Capture pre-prompt transcript baseline**

```bash
# Get message count before sending our test prompt
PRE_MSG_COUNT=$(curl -fsS "http://127.0.0.1:$SERVE_PORT/session/$GATE_SESSION_ID/messages" | jq 'length')
echo "Pre-prompt message count: $PRE_MSG_COUNT"

# Get the last assistant message to establish baseline agent identity
PRE_AGENT_TEXT=$(curl -fsS "http://127.0.0.1:$SERVE_PORT/session/$GATE_SESSION_ID/messages" \
  | jq -r '[.[] | select(.info.role == "assistant")][-1].parts[0].text // "NO_ASSISTANT_MSG"' | head -c 500)
echo "Pre-prompt last assistant message: $PRE_AGENT_TEXT"
```

Record `$PRE_MSG_COUNT` and `$PRE_AGENT_TEXT` as pre-prompt evidence.

- [ ] **Step 3: Send prompt_async WITHOUT agent field**

```bash
GATE_TIMESTAMP=$(date -u +%Y-%m-%dT%H:%M:%SZ)
HTTP_CODE=$(curl -s -o /dev/null -w '%{http_code}' -X POST \
  "http://127.0.0.1:$SERVE_PORT/session/$GATE_SESSION_ID/prompt_async" \
  -H "Content-Type: application/json" \
  -d "{
    \"parts\": [{
      \"type\": \"text\",
      \"text\": \"[E2E-GATE] Agent preservation test at $GATE_TIMESTAMP. Respond with: 1) your current skill/agent name, 2) the issue ID you are working on.\"
    }]
  }")
echo "prompt_async response: HTTP $HTTP_CODE"
```

Expected: HTTP 204. If not 204 → record error, retry once after 5s.

- [ ] **Step 4: Wait and evaluate agent preservation**

```bash
sleep 25

# Get all messages after our prompt
POST_MSGS=$(curl -fsS "http://127.0.0.1:$SERVE_PORT/session/$GATE_SESSION_ID/messages" \
  | jq "[.[$PRE_MSG_COUNT:]]")

# Find the assistant response to our gate prompt
GATE_RESPONSE=$(echo "$POST_MSGS" \
  | jq -r '[.[] | select(.info.role == "assistant")][0].parts[0].text // "NO_RESPONSE"' | head -c 1000)
echo "Gate response: $GATE_RESPONSE"
```

**PASS criteria (deterministic):** ALL of these must be true:
1. `$HTTP_CODE` was 204 (prompt accepted)
2. `$GATE_RESPONSE` is not "NO_RESPONSE" (session processed the prompt)
3. `$GATE_RESPONSE` contains at least one of: "legion", "worker", "skill", "plan", "implement", or the issue ID — indicating the session retained its agent context

**FAIL criteria:** Any of: HTTP code ≠ 204, no response within 60s, or response contains none of the agent-context markers listed above.

If no response after 25s, wait another 35s and re-check before declaring FAIL.

- [ ] **Step 5: Handle Step 0 failure (only if FAIL)**

If Step 0 FAILS:

```bash
gh issue comment 210 --body "## Step 0 Result: FAIL

**Environment:** $ENV_SNAPSHOT

### Evidence
- prompt_async HTTP code: $HTTP_CODE
- Gate response (first 500 chars): $(echo "$GATE_RESPONSE" | head -c 500)

### Blocker
Agent identity was NOT preserved through prompt_async without agent field. All AC1-AC5 results would be uninterpretable.

### Next Steps
File upstream OpenCode issue for prompt_async agent preservation." -R sjawhar/legion

gh issue edit 210 --add-label "user-input-needed" --remove-label "worker-active" -R sjawhar/legion
```

STOP. Do NOT proceed to Tasks 3-6.

---

### Task 3: Plan Worker Lifecycle — AC1 + AC4 + AC5 — Depends on: Task 2

This task covers three acceptance criteria using a single plan worker, tested sequentially: delivery (AC1), cross-mode cleanup (AC4), and resume re-subscription (AC5).

**Files:** None

#### Phase A: AC1 — Plan Worker Receives Issue Comment

- [ ] **Step 1: Ensure plan worker exists with subscription**

```bash
# Check if a plan worker for issue 210 already exists
PLAN_WORKER=$(curl -fsS "http://127.0.0.1:$DAEMON_PORT/workers" \
  | jq -r '.[] | select(.id == "sjawhar-legion-210-plan") // empty')

if [ -z "$PLAN_WORKER" ]; then
  # Dispatch a new plan worker
  PLAN_WORKER=$(curl -fsS -X POST "http://127.0.0.1:$DAEMON_PORT/workers" \
    -H "Content-Type: application/json" \
    -d '{
      "issueId": "sjawhar-legion-210",
      "mode": "plan",
      "repo": "sjawhar/legion",
      "issueNumber": 210,
      "prompt": "You are a plan worker for E2E Envoy verification. Wait for Envoy notifications."
    }')
  CREATED_WORKERS+=("sjawhar-legion-210-plan")
  sleep 5  # bootstrap + subscription delay
fi

export PLAN_SESSION_ID=$(echo "$PLAN_WORKER" | jq -r '.sessionId')
export PLAN_WORKER_ID="sjawhar-legion-210-plan"
echo "Plan worker session: $PLAN_SESSION_ID"
```

- [ ] **Step 2: Verify Envoy subscription**

```bash
AC1_SUB=$(curl -fsS "$ENVOY_URL/v1/interests/$PLAN_SESSION_ID" 2>/dev/null)
echo "AC1 subscription state: $AC1_SUB"

# Verify the issue topic is present
echo "$AC1_SUB" | jq -e '.topics[] | select(test("issue.210"))' > /dev/null 2>&1
if [ $? -ne 0 ]; then
  echo "WARNING: Issue topic not found. Waiting 5s and retrying..."
  sleep 5
  AC1_SUB=$(curl -fsS "$ENVOY_URL/v1/interests/$PLAN_SESSION_ID" 2>/dev/null)
  echo "Retry subscription state: $AC1_SUB"
fi
```

Expected: Topics contain `notifications.github.sjawhar.legion.issue.210.>`. Record `$AC1_SUB` as AC1 evidence (1).

- [ ] **Step 3: Post tagged comment to trigger delivery**

```bash
AC1_TIMESTAMP=$(date -u +%Y-%m-%dT%H:%M:%SZ)
gh issue comment $TEST_ISSUE --body "[E2E-AC1] $AC1_TIMESTAMP Verifying plan worker receives issue comment via Envoy auto-subscription." -R $OWNER/$REPO
echo "AC1 comment posted at $AC1_TIMESTAMP"
```

- [ ] **Step 4: Poll for delivery**

```bash
poll_transcript "$PLAN_SESSION_ID" "E2E-AC1"
AC1_DELIVERED=$?
# $AC1_DELIVERED: 0 = found (PASS), 1 = not found (FAIL)
# $MATCH contains the matched message text
AC1_EVIDENCE_DELIVERY="$MATCH"
```

**AC1 PASS:** `$AC1_DELIVERED` is 0 and `$MATCH` contains `[NOTIFICATION from github]` text with `[E2E-AC1]` tag.

#### Phase B: AC4 — Cross-Mode Subscription Cleanup

- [ ] **Step 5: Record pre-dispatch subscription state**

```bash
AC4_PRE_SUB=$(curl -fsS "$ENVOY_URL/v1/interests/$PLAN_SESSION_ID" 2>/dev/null)
echo "AC4 pre-dispatch plan worker subscription: $AC4_PRE_SUB"
```

Record `$AC4_PRE_SUB` as AC4 evidence (1).

- [ ] **Step 6: Dispatch implement worker for the same issue**

```bash
IMPL_DISPATCH=$(curl -fsS -X POST "http://127.0.0.1:$DAEMON_PORT/workers" \
  -H "Content-Type: application/json" \
  -d '{
    "issueId": "sjawhar-legion-210",
    "mode": "implement",
    "repo": "sjawhar/legion",
    "issueNumber": 210,
    "prompt": "You are an implement worker for E2E verification. Wait for instructions.",
    "force": true
  }' 2>&1)

# Handle 409 (worker already exists) — delete and retry
if echo "$IMPL_DISPATCH" | jq -e '.error == "worker_already_exists"' > /dev/null 2>&1; then
  echo "Implement worker already exists. Deleting and retrying..."
  curl -fsS -X DELETE "http://127.0.0.1:$DAEMON_PORT/workers/sjawhar-legion-210-implement"
  sleep 2
  IMPL_DISPATCH=$(curl -fsS -X POST "http://127.0.0.1:$DAEMON_PORT/workers" \
    -H "Content-Type: application/json" \
    -d '{
      "issueId": "sjawhar-legion-210",
      "mode": "implement",
      "repo": "sjawhar/legion",
      "issueNumber": 210,
      "prompt": "You are an implement worker for E2E verification. Wait for instructions.",
      "force": true
    }')
fi

IMPL_SESSION_ID=$(echo "$IMPL_DISPATCH" | jq -r '.sessionId')
CREATED_WORKERS+=("sjawhar-legion-210-implement")
echo "Implement worker dispatched. Session: $IMPL_SESSION_ID"
sleep 5  # allow dispatch processing
```

- [ ] **Step 7: Verify cleanup — plan worker unsubscribed, implement worker not subscribed**

```bash
# Plan worker should be unsubscribed (detachWorkerFromEnvoy was called)
AC4_POST_PLAN=$(curl -fsS "$ENVOY_URL/v1/interests/$PLAN_SESSION_ID" 2>/dev/null || echo '{"topics":[]}')
echo "AC4 plan worker post-dispatch: $AC4_POST_PLAN"

# Implement worker should NOT have issue subscriptions (subscribes only after PR creation)
AC4_IMPL_SUB=$(curl -fsS "$ENVOY_URL/v1/interests/$IMPL_SESSION_ID" 2>/dev/null || echo '{"topics":[]}')
echo "AC4 implement worker subscriptions: $AC4_IMPL_SUB"
```

**AC4 PASS:** Plan worker's issue topic is gone AND implement worker has no issue topics.

Record `$AC4_POST_PLAN` and `$AC4_IMPL_SUB` as AC4 evidence (2) and (3).

- [ ] **Step 8: Clean up implement worker**

```bash
curl -fsS -X DELETE "http://127.0.0.1:$DAEMON_PORT/workers/sjawhar-legion-210-implement" | jq .
```

#### Phase C: AC5 — Resume Re-Subscription

- [ ] **Step 9: Re-dispatch plan worker (AC4 destroyed its subscription)**

```bash
# The plan worker entry may still exist but without subscriptions.
# Check if it still exists:
PLAN_EXISTS=$(curl -fsS "http://127.0.0.1:$DAEMON_PORT/workers" \
  | jq -r '.[] | select(.id == "sjawhar-legion-210-plan") | .id // empty')

if [ -z "$PLAN_EXISTS" ]; then
  # Plan worker was removed — re-dispatch with version bump
  PLAN_REDISPATCH=$(curl -fsS -X POST "http://127.0.0.1:$DAEMON_PORT/workers" \
    -H "Content-Type: application/json" \
    -d '{
      "issueId": "sjawhar-legion-210",
      "mode": "plan",
      "repo": "sjawhar/legion",
      "issueNumber": 210,
      "version": 1,
      "prompt": "You are a plan worker for AC5 re-subscription test. Wait for instructions."
    }')
  PLAN_SESSION_ID=$(echo "$PLAN_REDISPATCH" | jq -r '.sessionId')
  sleep 5
fi
```

- [ ] **Step 10: Verify subscription is active, then remove it**

```bash
AC5_PRE_SUB=$(curl -fsS "$ENVOY_URL/v1/interests/$PLAN_SESSION_ID" 2>/dev/null)
echo "AC5 pre-removal subscription: $AC5_PRE_SUB"

# Verify topic exists
echo "$AC5_PRE_SUB" | jq -e '.topics[] | select(test("issue.210"))' > /dev/null 2>&1
if [ $? -ne 0 ]; then
  echo "ERROR: Plan worker has no issue subscription. AC5 cannot proceed."
  AC5_RESULT="FAIL (no subscription to remove)"
else
  # Remove ONLY the issue/PR topics (not all topics — preserve agent topic)
  curl -fsS -X POST "$ENVOY_URL/v1/interests/unsubscribe" \
    -H "Content-Type: application/json" \
    -d "{
      \"session_id\": \"$PLAN_SESSION_ID\",
      \"topics\": [
        \"notifications.github.sjawhar.legion.issue.210.>\",
        \"notifications.github.sjawhar.legion.pr.210.>\"
      ]
    }" | jq .
fi
```

Record `$AC5_PRE_SUB` as AC5 evidence (1).

- [ ] **Step 11: Verify subscription removed**

```bash
sleep 2
AC5_POST_REMOVE=$(curl -fsS "$ENVOY_URL/v1/interests/$PLAN_SESSION_ID" 2>/dev/null || echo '{"topics":[]}')
echo "AC5 post-removal: $AC5_POST_REMOVE"

# Confirm issue topic is gone
echo "$AC5_POST_REMOVE" | jq -e '.topics[] | select(test("issue.210"))' > /dev/null 2>&1
if [ $? -eq 0 ]; then
  echo "WARNING: Issue topic still present after unsubscribe"
fi
```

Record `$AC5_POST_REMOVE` as AC5 evidence (2).

- [ ] **Step 12: Resume worker and verify re-subscription**

```bash
# Resume via daemon prompt endpoint — this triggers re-subscription
curl -fsS -X POST "http://127.0.0.1:$DAEMON_PORT/workers/$PLAN_WORKER_ID/prompt" \
  -H "Content-Type: application/json" \
  -d '{"text": "[E2E-AC5] Resume re-subscription test."}' | jq .

sleep 5

AC5_POST_RESUME=$(curl -fsS "$ENVOY_URL/v1/interests/$PLAN_SESSION_ID" 2>/dev/null)
echo "AC5 post-resume subscription: $AC5_POST_RESUME"
```

**AC5 PASS:** `$AC5_POST_RESUME` topics contain `notifications.github.sjawhar.legion.issue.210.>`.

If not found after 5s, wait 5 more seconds and retry once.

Record `$AC5_POST_RESUME` as AC5 evidence (3).

---

### Task 4: AC2 — PR Topic Delivery — Depends on: Task 2

This task verifies that a session subscribed to PR topics receives PR comment notifications. The issue AC says "Implementer receives PR review comment after self-subscription." In practice, we verify the delivery path: subscribe → PR event → notification arrives.

**Strategy:** Find an existing implement worker that self-subscribed to a PR topic. If none exists, use a dedicated session with a manual Envoy subscription. When using manual subscription, record this in the evidence and note that the self-subscription path is not covered.

**Files:** None

- [ ] **Step 1: Find a session subscribed to PR topics**

```bash
# Look for any implement worker with PR topic subscriptions
IMPL_WITH_PR=$(curl -fsS "http://127.0.0.1:$DAEMON_PORT/workers" \
  | jq -r '[.[] | select(.envoyTopics[]? | test("pr\\."))] | .[0] // empty')

if [ -n "$IMPL_WITH_PR" ]; then
  AC2_SESSION_ID=$(echo "$IMPL_WITH_PR" | jq -r '.sessionId')
  AC2_WORKER_ID=$(echo "$IMPL_WITH_PR" | jq -r '.id')
  AC2_METHOD="self-subscribed"
  # Extract PR number from topics
  PR_NUMBER=$(echo "$IMPL_WITH_PR" | jq -r '.envoyTopics[] | capture("pr\\.(?<n>[0-9]+)") | .n' | head -1)
  echo "Found implement worker $AC2_WORKER_ID subscribed to PR #$PR_NUMBER"
else
  echo "No implement worker with PR subscription found."
  echo "Using manual subscription on a dedicated session."
  AC2_METHOD="manual-subscription"

  # Find an open PR to use
  PR_NUMBER=$(gh pr list -R $OWNER/$REPO --json number -q '.[0].number' 2>/dev/null)
  if [ -z "$PR_NUMBER" ]; then
    echo "ERROR: No open PRs found. AC2 cannot proceed. Recording as SKIP."
    AC2_RESULT="SKIP (no open PRs and no implement worker with PR subscription)"
  else
    # Dispatch a dedicated worker for AC2
    AC2_DISPATCH=$(curl -fsS -X POST "http://127.0.0.1:$DAEMON_PORT/workers" \
      -H "Content-Type: application/json" \
      -d "{
        \"issueId\": \"sjawhar-legion-210\",
        \"mode\": \"test\",
        \"repo\": \"sjawhar/legion\",
        \"issueNumber\": 210,
        \"prompt\": \"You are a test worker for AC2 PR delivery verification. Wait for notifications.\",
        \"force\": true
      }")
    AC2_SESSION_ID=$(echo "$AC2_DISPATCH" | jq -r '.sessionId')
    AC2_WORKER_ID="sjawhar-legion-210-test"
    CREATED_WORKERS+=("$AC2_WORKER_ID")
    sleep 5

    # Manually subscribe to the PR topic
    curl -fsS -X POST "$ENVOY_URL/v1/interests/subscribe" \
      -H "Content-Type: application/json" \
      -d "{
        \"session_id\": \"$AC2_SESSION_ID\",
        \"topics\": [\"notifications.github.$OWNER.$REPO.pr.$PR_NUMBER.>\"]
      }" | jq .
    echo "Manually subscribed $AC2_SESSION_ID to PR #$PR_NUMBER"
  fi
fi

echo "AC2: session=$AC2_SESSION_ID method=$AC2_METHOD PR=#$PR_NUMBER"
```

- [ ] **Step 2: Verify PR subscription in Envoy**

```bash
AC2_SUB=$(curl -fsS "$ENVOY_URL/v1/interests/$AC2_SESSION_ID" 2>/dev/null)
echo "AC2 subscription: $AC2_SUB"
```

Expected: Topics contain `notifications.github.sjawhar.legion.pr.$PR_NUMBER.>`. Record as AC2 evidence (1).

- [ ] **Step 3: Post PR comment to trigger delivery**

```bash
AC2_TIMESTAMP=$(date -u +%Y-%m-%dT%H:%M:%SZ)

# Use a simple PR comment (issue-style on PR). This generates a
# notifications.github.sjawhar.legion.pr.<N>.comment event.
gh pr comment "$PR_NUMBER" \
  --body "[E2E-AC2] $AC2_TIMESTAMP Verifying PR topic delivery via Envoy." \
  -R $OWNER/$REPO

echo "AC2 comment posted on PR #$PR_NUMBER at $AC2_TIMESTAMP"
```

Note: We use `gh pr comment` which generates a PR issue comment event. The Envoy receiver normalizes this to the PR topic. If the AC strictly requires a PR review comment (line-level), and the PR has no diff, this is the closest deliverable event.

- [ ] **Step 4: Poll for delivery**

```bash
poll_transcript "$AC2_SESSION_ID" "E2E-AC2"
AC2_DELIVERED=$?
AC2_EVIDENCE_DELIVERY="$MATCH"
```

**AC2 PASS:** `$AC2_DELIVERED` is 0 and notification contains `[E2E-AC2]` tag.

---

### Task 5: AC3 — Controller Receives @legion Mention — Depends on: Task 2

**Files:** None

- [ ] **Step 1: Find the controller session**

```bash
# Controller subscribes to notifications.github.*.*.mention (or similar)
# Find it by scanning Envoy interests for a session with mention topics
CTRL_INTEREST=$(curl -fsS "$ENVOY_URL/v1/interests/" \
  | jq -r '[.[] | select(.topics[]? | test("mention"))][0] // empty')

if [ -z "$CTRL_INTEREST" ]; then
  echo "ERROR: No session subscribed to mention topics found in Envoy."
  echo "AC3 FAIL — controller not subscribed to mentions."
  AC3_RESULT="FAIL (no session subscribed to mention topics)"
  CTRL_SESSION_ID=""
else
  CTRL_SESSION_ID=$(echo "$CTRL_INTEREST" | jq -r '.session_id')
  echo "Controller session: $CTRL_SESSION_ID"
fi
```

If `$CTRL_SESSION_ID` is empty, skip Steps 2-4 and record AC3 as FAIL.

- [ ] **Step 2: Record controller subscription state**

```bash
AC3_SUB=$(curl -fsS "$ENVOY_URL/v1/interests/$CTRL_SESSION_ID" 2>/dev/null)
echo "AC3 controller subscription: $AC3_SUB"
```

Record `$AC3_SUB` as AC3 evidence (1).

- [ ] **Step 3: Post @legion tagged comment**

```bash
AC3_TIMESTAMP=$(date -u +%Y-%m-%dT%H:%M:%SZ)
gh issue comment $TEST_ISSUE \
  --body "[E2E-AC3] $AC3_TIMESTAMP @legion Testing mention delivery to controller via Envoy auto-subscription." \
  -R $OWNER/$REPO
echo "AC3 mention posted at $AC3_TIMESTAMP"
```

- [ ] **Step 4: Poll for delivery**

```bash
poll_transcript "$CTRL_SESSION_ID" "E2E-AC3"
AC3_DELIVERED=$?
AC3_EVIDENCE_DELIVERY="$MATCH"
```

**AC3 PASS:** `$AC3_DELIVERED` is 0 and notification contains `[E2E-AC3]` tag.

---

### Task 6: Final Report & Cleanup — Depends on: Task 2, Task 3, Task 4, Task 5

**Files:** None

- [ ] **Step 1: Compile and post consolidated evidence report**

Assemble results from all tasks into one issue comment. Replace each placeholder with the actual captured value from the corresponding task.

```bash
# Determine verdicts from captured evidence
STEP0_VERDICT="[PASS or FAIL based on Task 2 Step 4 criteria]"
AC1_VERDICT="[PASS or FAIL based on Task 3 Phase A]"
AC4_VERDICT="[PASS or FAIL based on Task 3 Phase B]"
AC5_VERDICT="[PASS or FAIL based on Task 3 Phase C]"
AC2_VERDICT="[PASS or FAIL based on Task 4, or SKIP]"
AC3_VERDICT="[PASS or FAIL based on Task 5]"

gh issue comment $TEST_ISSUE --body "$(cat <<'REPORT_EOF'
## E2E Envoy Auto-Subscription Verification Report

**Environment:** $ENV_SNAPSHOT

| # | Criterion | Result | Key Evidence |
|---|-----------|--------|--------------|
| 0 | Agent Preservation Gate | $STEP0_VERDICT | prompt_async without agent field; response retained agent context |
| 1 | Issue Comment Delivery (AC1) | $AC1_VERDICT | Plan worker received [E2E-AC1] notification |
| 2 | PR Comment Delivery (AC2) | $AC2_VERDICT | Session received [E2E-AC2] notification (method: $AC2_METHOD) |
| 3 | @mention Delivery (AC3) | $AC3_VERDICT | Controller received [E2E-AC3] notification |
| 4 | Cross-Mode Cleanup (AC4) | $AC4_VERDICT | Plan subscription removed after implement dispatch |
| 5 | Resume Re-Subscription (AC5) | $AC5_VERDICT | Subscription restored after removal + daemon resume |

### Step 0 Evidence
- HTTP response code: $HTTP_CODE
- Gate response excerpt: [first 300 chars of $GATE_RESPONSE]

### AC1 Evidence
- Subscription before trigger: $AC1_SUB
- Delivered notification excerpt: [first 300 chars of $AC1_EVIDENCE_DELIVERY]

### AC2 Evidence
- Subscription method: $AC2_METHOD
- Subscription state: $AC2_SUB
- PR number: #$PR_NUMBER
- Delivered notification excerpt: [first 300 chars of $AC2_EVIDENCE_DELIVERY]

### AC3 Evidence
- Controller subscription: $AC3_SUB
- Delivered notification excerpt: [first 300 chars of $AC3_EVIDENCE_DELIVERY]

### AC4 Evidence
- Plan sub BEFORE implement dispatch: $AC4_PRE_SUB
- Plan sub AFTER implement dispatch: $AC4_POST_PLAN
- Implement sub AFTER dispatch: $AC4_IMPL_SUB

### AC5 Evidence
- Subscription BEFORE removal: $AC5_PRE_SUB
- Subscription AFTER removal: $AC5_POST_REMOVE
- Subscription AFTER resume: $AC5_POST_RESUME

### Observations
[Timing patterns, anomalies, recommendations for automated testing]
REPORT_EOF
)" -R $OWNER/$REPO
```

Note: The implementer must substitute shell variables into the heredoc. If the shell doesn't expand variables in `<<'REPORT_EOF'` (single-quoted heredoc), use `<<REPORT_EOF` (unquoted) instead, or build the body string programmatically.

- [ ] **Step 2: Clean up test workers**

Delete only workers created by this plan:

```bash
for WORKER_ID in "${CREATED_WORKERS[@]}"; do
  echo "Cleaning up: $WORKER_ID"
  curl -fsS -X DELETE "http://127.0.0.1:$DAEMON_PORT/workers/$WORKER_ID" 2>/dev/null | jq .
done
```

---

## Dependency Graph

```
Task 1: Environment Discovery — Independent
Task 2: Step 0 Agent Gate — Depends on: Task 1
Task 3: Plan Worker Lifecycle (AC1+AC4+AC5) — Depends on: Task 2
Task 4: AC2 PR Topic Delivery — Depends on: Task 2
Task 5: AC3 @mention Delivery — Depends on: Task 2
Task 6: Final Report & Cleanup — Depends on: Task 2, Task 3, Task 4, Task 5
```

Tasks 3, 4, and 5 can execute in parallel after Task 2 passes (they use independent sessions). Task 6 waits for everything.

---

## Testing Plan

### Setup
- Shared serve running: `curl -fsS http://127.0.0.1:${SERVE_PORT:-13381}/global/health`
- Daemon running: `curl -fsS http://127.0.0.1:${LEGION_DAEMON_PORT:-13370}/health`
- Envoy reachable: `curl -fsS ${ENVOY_URL:-http://127.0.0.1:9020}/v1/interests/`
- GitHub CLI authenticated: `gh auth status`

### Health Check
- Retry each health check for 30s before declaring failure
- All three services must be healthy before proceeding

### Verification Steps
For each acceptance criterion:
1. **Precondition:** Verify subscription state via `GET $ENVOY_URL/v1/interests/{sessionId}`
2. **Trigger:** Post tagged comment/mention via GitHub CLI
3. **Observe:** Poll full session transcript for tag match (60s window, 10s intervals)
4. **Evidence:** Capture API responses and transcript excerpts as shell variables
5. **Verdict:** PASS requires concrete runtime evidence per the criteria defined in each task

### Tools Needed
- curl (API calls to daemon, Envoy, serve)
- jq (JSON parsing)
- gh CLI (GitHub issue/PR comments)
- bash (poll helper, variable tracking)

### Failure Diagnosis
If delivery fails, use `systematic-debugging` skill to trace which layer broke:
1. **Subscription:** `GET $ENVOY_URL/v1/interests/{sessionId}` — subscribed?
2. **Webhook:** Did GitHub fire the webhook? Check Envoy receiver logs.
3. **NATS:** Did the message reach JetStream? Check listener logs.
4. **Routing:** Did the listener match the topic? Check machine_id alignment.
5. **Delivery:** Did prompt_async return 204? Check serve logs.
6. **Session:** Did the session process the prompt? Check transcript.

## Required Skills

The following project-specific skills should be loaded by downstream workers:

| Phase | Skills |
|-------|--------|
| Implement | `envoy`, `systematic-debugging` |
| Test | `envoy` |
| Review | — |

Workers: invoke these skills at the start of your workflow before beginning work.
