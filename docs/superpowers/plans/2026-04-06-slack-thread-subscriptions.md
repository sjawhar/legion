# Slack Thread-Level Subscriptions Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Enable Envoy subscribers to independently subscribe to specific Slack threads by normalizing `thread_ts` in NATS topics and adding kind suffixes.

**Architecture:** Contract-shape-first change. Add a `slackThreadSubject` TS helper that normalizes `thread_ts` (`.` → `_`) and appends a kind suffix. Regenerate Go equivalent. Update `SlackEnvelopes()` to use the new helper and remove the `ts` fallback. Prove routing isolation and channel wildcard compatibility through targeted tests. Document the new topic hierarchy.

**Tech Stack:** TypeScript (Bun), Go, NATS topic conventions, `@legion/contracts` generation pipeline

**Required Skills:** `envoy` (topic format conventions)

---

## File Structure

| Action | File | Responsibility |
|--------|------|----------------|
| Modify | `packages/contracts/src/subject.ts` | Add `slackThreadSubject` helper |
| Modify | `packages/contracts/scripts/gen-go.ts` | Add `SlackThreadSubject` to keep block |
| Regenerate | `packages/envoy/internal/contracts/generated.go` | Auto-generated from gen-go.ts |
| Modify | `packages/envoy/internal/contracts/normalize.go` | Update `SlackEnvelopes()`: use new helper, remove `ts` fallback, add kind |
| Modify | `packages/contracts/src/envelope.test.ts` | TS tests for `slackThreadSubject` |
| Modify | `packages/envoy/internal/contracts/normalize_test.go` | Go tests for updated `SlackEnvelopes()` |
| Modify | `packages/envoy/internal/routing/match_test.go` | Filtering proof: thread topic isolation |
| Modify | `packages/envoy/internal/store/kv_test.go` | Subscription-level proof: thread isolation + channel wildcard |
| Modify | `packages/contracts/README.md` | Slack topic hierarchy documentation |
| Modify | `.opencode/skills/envoy/SKILL.md` | Thread subscription patterns in skill docs |

---

## Task 1: Add `slackThreadSubject` TS helper + tests — Independent

**Files:**
- Modify: `packages/contracts/src/subject.ts`
- Modify: `packages/contracts/src/envelope.test.ts`

- [ ] **Step 1: Write failing tests for `slackThreadSubject`**

Add to `packages/contracts/src/envelope.test.ts` — add the import first, then the test block at the end of the file:

```typescript
// Add to imports at top of file (line 3-9):
import {
  GHOSTWISPR_TOPIC_PREFIX,
  ghostWisprSubject,
  githubResourceSubject,
  githubSubject,
  slackSubject,
  slackThreadSubject,
  whatsappSubject,
} from "./subject";

// Add new describe block at end of file:
describe("slackThreadSubject", () => {
  test("normalizes thread_ts dot to underscore", () => {
    expect(slackThreadSubject("T123", "C456", "1234567890.123456", "message")).toBe(
      "notifications.slack.T123.C456.thread.1234567890_123456.message"
    );
  });

  test("returns mention kind for app_mention threads", () => {
    expect(slackThreadSubject("T123", "C456", "1234567890.123456", "mention")).toBe(
      "notifications.slack.T123.C456.thread.1234567890_123456.mention"
    );
  });

  test("is consistent with slackSubject prefix", () => {
    const thread = slackThreadSubject("T123", "C456", "1234567890.123456", "message");
    const channel = slackSubject("T123", "C456", "thread");
    expect(thread.startsWith(`${channel}.`)).toBe(true);
  });
});
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `bun test packages/contracts/src/envelope.test.ts`
Expected: FAIL — `slackThreadSubject` is not exported from `./subject`

- [ ] **Step 3: Implement `slackThreadSubject` in subject.ts**

Add after `slackSubject` (after line 13 in `packages/contracts/src/subject.ts`):

```typescript
export function slackThreadSubject(
  team: string,
  channel: string,
  threadTs: string,
  kind: string
) {
  return `notifications.slack.${team}.${channel}.thread.${threadTs.replaceAll(".", "_")}.${kind}`;
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `bun test packages/contracts/src/envelope.test.ts`
Expected: All tests PASS including the 3 new `slackThreadSubject` tests

- [ ] **Step 5: Describe and advance**

```bash
jj describe -m "feat(contracts): add slackThreadSubject helper with dot normalization"
jj new
```

---

## Task 2: Add `SlackThreadSubject` Go helper + regenerate — Depends on: Task 1

**Files:**
- Modify: `packages/contracts/scripts/gen-go.ts` (keep block)
- Regenerate: `packages/envoy/internal/contracts/generated.go`

- [ ] **Step 1: Add `SlackThreadSubject` to the keep block in gen-go.ts**

In `packages/contracts/scripts/gen-go.ts`, add after the `SlackSubject` function (after line 43). The new function goes inside the `keep` template literal.

**CRITICAL: Use literal tab characters for indentation, not spaces.** The keep block is written verbatim to `generated.go` and must pass `gofmt`.

Add between the `SlackSubject` closing brace (line 43 `}`) and the blank line before `GithubResourceSubject` (line 44):

```go

func SlackThreadSubject(team, channel, threadTs, kind string) string {
	return "notifications.slack." + team + "." + channel + ".thread." + strings.ReplaceAll(threadTs, ".", "_") + "." + kind
}
```

Note: `strings` is already imported in the generated file template (line 127-128 of gen-go.ts).

- [ ] **Step 2: Regenerate the Go contract file**

Run: `bun run packages/contracts/scripts/gen-go.ts`
Expected: Exit code 0, `packages/envoy/internal/contracts/generated.go` updated

- [ ] **Step 3: Verify gofmt is clean**

Run: `gofmt -l packages/envoy/internal/contracts/generated.go`
Expected: No output (empty = file is correctly formatted). If the filename is printed, the keep block has spaces instead of tabs.

- [ ] **Step 4: Verify Go compiles**

Run from `packages/envoy`: `go build ./...`
Expected: Exit code 0, no errors

- [ ] **Step 5: Describe and advance**

```bash
jj describe -m "feat(contracts): add SlackThreadSubject Go helper via generation pipeline"
jj new
```

---

## Task 3: Update `SlackEnvelopes()` + Go normalize tests — Depends on: Task 2

**Files:**
- Modify: `packages/envoy/internal/contracts/normalize.go`
- Modify: `packages/envoy/internal/contracts/normalize_test.go`

- [ ] **Step 1: Write failing Go tests for the new behavior**

In `packages/envoy/internal/contracts/normalize_test.go`, **replace** `TestSlackEnvelopesThread` (lines 333-358) and `TestSlackEnvelopesNoThread` (lines 360-386) with updated versions:

```go
func TestSlackEnvelopesThread(t *testing.T) {
	items := SlackEnvelopes(SlackEnvelopeInput{
		EventID: "e1",
		TraceID: "t1",
		Body: map[string]any{
			"team_id":  "T123",
			"event_id": "Ev123",
			"event": map[string]any{
				"type":      "message",
				"user":      "U123",
				"channel":   "C123",
				"text":      "reply in thread",
				"thread_ts": "1234567890.123456",
			},
		},
	})
	if len(items) != 2 {
		t.Fatalf("expected 2 envelopes, got %d", len(items))
	}
	if items[0].Topic != "notifications.slack.T123.C123.message" {
		t.Fatalf("unexpected channel topic: %s", items[0].Topic)
	}
	// Thread topic must use normalized ts (dot→underscore) and kind suffix
	if items[1].Topic != "notifications.slack.T123.C123.thread.1234567890_123456.message" {
		t.Fatalf("unexpected thread topic: %s", items[1].Topic)
	}
	// Both envelopes share the same dedupe_key (critical for single-delivery guarantee)
	if items[0].DedupeKey != items[1].DedupeKey {
		t.Fatalf("dedupe keys differ: channel=%s thread=%s", items[0].DedupeKey, items[1].DedupeKey)
	}
}

func TestSlackEnvelopesThreadMention(t *testing.T) {
	items := SlackEnvelopes(SlackEnvelopeInput{
		EventID: "e1",
		TraceID: "t1",
		Body: map[string]any{
			"team_id":  "T123",
			"event_id": "Ev123",
			"event": map[string]any{
				"type":      "app_mention",
				"user":      "U123",
				"channel":   "C123",
				"text":      "@bot help in thread",
				"thread_ts": "1234567890.123456",
			},
		},
	})
	if len(items) != 2 {
		t.Fatalf("expected 2 envelopes, got %d", len(items))
	}
	if items[0].Topic != "notifications.slack.T123.C123.mention" {
		t.Fatalf("unexpected channel topic: %s", items[0].Topic)
	}
	// Thread mention must use .mention kind suffix
	if items[1].Topic != "notifications.slack.T123.C123.thread.1234567890_123456.mention" {
		t.Fatalf("unexpected thread topic: %s", items[1].Topic)
	}
}

func TestSlackEnvelopesNoThread(t *testing.T) {
	items := SlackEnvelopes(SlackEnvelopeInput{
		EventID: "e1",
		TraceID: "t1",
		Body: map[string]any{
			"team_id":  "T123",
			"event_id": "Ev123",
			"event": map[string]any{
				"type":    "app_mention",
				"user":    "U123",
				"channel": "C123",
				"text":    "hello",
				"ts":      "9999999999.000000",
			},
		},
	})
	// No thread_ts present — only channel-level envelope, NO ts fallback
	if len(items) != 1 {
		t.Fatalf("expected 1 envelope (no thread fallback), got %d", len(items))
	}
	if items[0].Topic != "notifications.slack.T123.C123.mention" {
		t.Fatalf("unexpected channel topic: %s", items[0].Topic)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run from `packages/envoy`: `go test ./internal/contracts/ -run 'TestSlackEnvelopes'`
Expected: FAIL — `TestSlackEnvelopesThread` expects normalized topic, `TestSlackEnvelopesNoThread` expects 1 envelope but gets 2

- [ ] **Step 3: Update `SlackEnvelopes()` in normalize.go**

Replace the `SlackEnvelopes` function (lines 150-168 in `packages/envoy/internal/contracts/normalize.go`) with:

```go
func SlackEnvelopes(input SlackEnvelopeInput) []Envelope {
	item := SlackEnvelope(input)
	out := []Envelope{item}
	event, _ := input.Body["event"].(map[string]any)
	thread := stringValue(event["thread_ts"])
	if thread != "" {
		team := stringValue(input.Body["team_id"])
		channel := stringValue(event["channel"])
		if team != "" && channel != "" {
			threaded := item
			threaded.Topic = SlackThreadSubject(team, channel, thread, slackKind(input.Body))
			out = append(out, threaded)
		}
	}
	return out
}
```

Changes from current code:
1. **Removed `ts` fallback** (deleted lines 155-157: `if thread == "" { thread = stringValue(event["ts"]) }`)
2. **Use `SlackThreadSubject()`** instead of `SlackSubject(team, channel, "thread."+thread)` — this normalizes the dot and appends kind
3. **Pass `slackKind(input.Body)`** as the kind parameter — reuses the existing kind determination

- [ ] **Step 4: Run tests to verify they pass**

Run from `packages/envoy`: `go test ./internal/contracts/ -run 'TestSlackEnvelopes'`
Expected: All 3 Slack envelope tests PASS

- [ ] **Step 5: Run full normalize test suite**

Run from `packages/envoy`: `go test ./internal/contracts/`
Expected: All tests PASS (no regressions in GitHub/GhostWispr/WhatsApp tests)

- [ ] **Step 6: Describe and advance**

```bash
jj describe -m "feat(envoy): normalize slack thread topics with kind suffix, remove ts fallback"
jj new
```

---

## Task 4: Add routing filtering proof tests — Depends on: Task 2

**Files:**
- Modify: `packages/envoy/internal/routing/match_test.go`

- [ ] **Step 1: Add thread filtering proof test**

Add to `packages/envoy/internal/routing/match_test.go` after `TestPerPRFiltering`:

```go
func TestPerSlackThreadFiltering(t *testing.T) {
	pattern := "notifications.slack.T123.C456.thread.1234567890_123456.>"
	cases := []struct {
		topic string
		ok    bool
	}{
		// Should match: target thread and its subtopics
		{topic: "notifications.slack.T123.C456.thread.1234567890_123456.message", ok: true},
		{topic: "notifications.slack.T123.C456.thread.1234567890_123456.mention", ok: true},
		// Should NOT match: different thread
		{topic: "notifications.slack.T123.C456.thread.9999999999_000000.message", ok: false},
		{topic: "notifications.slack.T123.C456.thread.9999999999_000000.mention", ok: false},
		// Should NOT match: channel-level topic (no thread segment)
		{topic: "notifications.slack.T123.C456.message", ok: false},
		{topic: "notifications.slack.T123.C456.mention", ok: false},
		// Should NOT match: different channel
		{topic: "notifications.slack.T123.C789.thread.1234567890_123456.message", ok: false},
	}
	for _, item := range cases {
		got := Match(pattern, item.topic)
		if got != item.ok {
			t.Fatalf("pattern=%s topic=%s expected=%v got=%v", pattern, item.topic, item.ok, got)
		}
	}

	// Prove normalized thread_ts (underscore) is a single NATS segment
	// while raw thread_ts (dot) would be two segments
	starPattern := "notifications.slack.T.C.thread.*.message"
	// Normalized ts = single segment: matches *
	if !Match(starPattern, "notifications.slack.T.C.thread.1234567890_123456.message") {
		t.Fatal("normalized thread_ts should match single-segment wildcard")
	}
	// Raw ts (dot) = two segments: does NOT match *
	if Match(starPattern, "notifications.slack.T.C.thread.1234567890.123456.message") {
		t.Fatal("raw dotted thread_ts should NOT match single-segment wildcard")
	}
}
```

- [ ] **Step 2: Run tests to verify they pass**

Run from `packages/envoy`: `go test ./internal/routing/ -run 'TestPerSlackThreadFiltering'`
Expected: PASS — proves thread isolation + single-segment wildcard matching

- [ ] **Step 3: Describe and advance**

```bash
jj describe -m "test(envoy): prove slack thread routing isolation with normalized timestamps"
jj new
```

---

## Task 5: Add subscription-level isolation tests — Depends on: Task 2

**Files:**
- Modify: `packages/envoy/internal/store/kv_test.go`

- [ ] **Step 1: Add thread subscription isolation test**

Add to `packages/envoy/internal/store/kv_test.go` after `TestMatch_WildcardPatterns`:

```go
func TestMatch_SlackThreadIsolation(t *testing.T) {
	r := &Registry{cache: map[string]Interest{
		"ses_thread_a": {SessionID: "ses_thread_a", MachineID: "m1", Topics: []string{
			"notifications.slack.T123.C456.thread.1111111111_111111.>",
		}},
		"ses_thread_b": {SessionID: "ses_thread_b", MachineID: "m1", Topics: []string{
			"notifications.slack.T123.C456.thread.2222222222_222222.>",
		}},
		"ses_channel": {SessionID: "ses_channel", MachineID: "m1", Topics: []string{
			"notifications.slack.T123.C456.>",
		}},
	}}

	// Thread A message: only ses_thread_a and ses_channel
	gotA := r.Match("m1", "notifications.slack.T123.C456.thread.1111111111_111111.message")
	idsA := make([]string, len(gotA))
	for i, item := range gotA {
		idsA[i] = item.SessionID
	}
	sort.Strings(idsA)
	if len(idsA) != 2 || idsA[0] != "ses_channel" || idsA[1] != "ses_thread_a" {
		t.Fatalf("thread A message: expected [ses_channel, ses_thread_a], got %v", idsA)
	}

	// Thread B mention: only ses_thread_b and ses_channel
	gotB := r.Match("m1", "notifications.slack.T123.C456.thread.2222222222_222222.mention")
	idsB := make([]string, len(gotB))
	for i, item := range gotB {
		idsB[i] = item.SessionID
	}
	sort.Strings(idsB)
	if len(idsB) != 2 || idsB[0] != "ses_channel" || idsB[1] != "ses_thread_b" {
		t.Fatalf("thread B mention: expected [ses_channel, ses_thread_b], got %v", idsB)
	}

	// Channel-level message (no thread): only ses_channel
	gotC := r.Match("m1", "notifications.slack.T123.C456.message")
	idsC := make([]string, len(gotC))
	for i, item := range gotC {
		idsC[i] = item.SessionID
	}
	if len(idsC) != 1 || idsC[0] != "ses_channel" {
		t.Fatalf("channel message: expected [ses_channel], got %v", idsC)
	}
}
```

Note: add `"sort"` to the imports at the top of kv_test.go if not already present.

- [ ] **Step 2: Run tests to verify they pass**

Run from `packages/envoy`: `go test ./internal/store/ -run 'TestMatch_SlackThreadIsolation'`
Expected: PASS — thread A subscriber only sees thread A events, thread B subscriber only sees thread B events, channel subscriber sees all

- [ ] **Step 3: Describe and advance**

```bash
jj describe -m "test(envoy): prove slack thread subscription isolation at store level"
jj new
```

---

## Task 6: Update documentation — Depends on: Task 3

**Files:**
- Modify: `packages/contracts/README.md`
- Modify: `.opencode/skills/envoy/SKILL.md`

- [ ] **Step 1: Add Slack topic hierarchy to README**

Add to `packages/contracts/README.md` after the GitHub section (after line 51, the "Not yet published" section). Follow the exact same documentation pattern used for GitHub:

```markdown

## Slack Topic Hierarchy

The Slack receiver publishes channel-level and thread-level topics. Consumers
subscribe at the granularity they need using wildcard patterns.

### Base subjects

| Helper | Example output |
|--------|---------------|
| `slackSubject(team, channel, kind)` | `notifications.slack.T09FRELLTS8.C0A0DHVU8HE.message` |
| `slackThreadSubject(team, channel, threadTs, kind)` | `notifications.slack.T09FRELLTS8.C0A0DHVU8HE.thread.1234567890_123456.message` |

### Published topic patterns

| Event type | Published topic(s) |
|-----------|-------------------|
| Channel message | `notifications.slack.{team}.{channel}.message` |
| Channel mention (`app_mention`) | `notifications.slack.{team}.{channel}.mention` |
| Thread reply (message) | Channel topic + `notifications.slack.{team}.{channel}.thread.{normalized_ts}.message` |
| Thread mention (`app_mention` in thread) | Channel topic + `notifications.slack.{team}.{channel}.thread.{normalized_ts}.mention` |
| Standalone message (no `thread_ts`) | Channel topic only (no thread envelope) |

**Thread timestamp normalization:** Slack `thread_ts` values contain dots
(e.g., `1234567890.123456`) which conflict with NATS segment separators. The
`slackThreadSubject` helper normalizes by replacing `.` with `_`
(→ `1234567890_123456`), making the thread identifier a single NATS segment.

### Subscription granularity

| Want | Subscribe to |
|------|-------------|
| All events in channel | `notifications.slack.T.C.>` |
| All messages in channel | `notifications.slack.T.C.message` |
| All mentions in channel | `notifications.slack.T.C.mention` |
| All events in specific thread | `notifications.slack.T.C.thread.1234567890_123456.>` |
| Only messages in thread | `notifications.slack.T.C.thread.1234567890_123456.message` |
| Only mentions in thread | `notifications.slack.T.C.thread.1234567890_123456.mention` |
| All threads in channel | `notifications.slack.T.C.thread.>` |

### Deduplication

A session subscribed to both `notifications.slack.T.C.>` and a specific thread
topic receives each event **once** (not duplicated), because both envelopes
share the same `dedupe_key` and the listener deduplicates by
`(dedupe_key, session_id)`.
```

- [ ] **Step 2: Update Envoy skill docs with thread subscription patterns**

In `.opencode/skills/envoy/SKILL.md`, find the existing Slack topic section (around lines 58-72 based on the explore results) and replace the thread topic entry with the updated format. Also add subscription examples:

Update the thread topic entry from:
```
notifications.slack.<team_id>.<channel_id>.thread.<thread_ts>
```
to:
```
notifications.slack.<team_id>.<channel_id>.thread.<normalized_ts>.message
notifications.slack.<team_id>.<channel_id>.thread.<normalized_ts>.mention
```

Add a note about normalization:
```
Thread timestamps are normalized: `1234567890.123456` → `1234567890_123456`
(dots replaced with underscores to make the thread ID a single NATS segment).
```

Add subscription examples for threads (following the existing pattern for mentions):
```
# Subscribe to all events in a specific thread
envoy_subscribe(["notifications.slack.T09FRELLTS8.C0A0DHVU8HE.thread.1234567890_123456.>"])

# Subscribe to only messages in a thread (not mentions)
envoy_subscribe(["notifications.slack.T09FRELLTS8.C0A0DHVU8HE.thread.1234567890_123456.message"])

# Subscribe to all threads in a channel
envoy_subscribe(["notifications.slack.T09FRELLTS8.C0A0DHVU8HE.thread.>"])
```

- [ ] **Step 3: Verify documentation contains expected patterns**

Run these checks to verify the key patterns are present in both files:

```bash
# README must document slackThreadSubject helper
grep -q 'slackThreadSubject' packages/contracts/README.md && echo 'PASS: README has slackThreadSubject' || echo 'FAIL'

# README must show normalized thread topic
grep -q 'thread.1234567890_123456.message' packages/contracts/README.md && echo 'PASS: README has normalized thread topic' || echo 'FAIL'

# README must document deduplication behavior
grep -q 'dedupe_key' packages/contracts/README.md && echo 'PASS: README has dedupe docs' || echo 'FAIL'

# Envoy skill must show normalized thread topics (not raw dotted format)
grep -q 'thread.<normalized_ts>.message' .opencode/skills/envoy/SKILL.md && echo 'PASS: Skill has normalized thread format' || echo 'FAIL'

# Envoy skill must have thread subscription examples
grep -q 'thread.1234567890_123456.>' .opencode/skills/envoy/SKILL.md && echo 'PASS: Skill has thread subscription example' || echo 'FAIL'
```

Expected: All 5 checks print PASS

- [ ] **Step 4: Describe and advance**

```bash
jj describe -m "docs(envoy): document slack thread topic hierarchy and subscription patterns"
jj new
```

---

## Dependency Graph

```
Task 1 (TS helper) ──→ Task 2 (Go helper) ──┬──→ Task 3 (normalize)
                                             ├──→ Task 4 (routing tests)
                                             └──→ Task 5 (store tests)
                       Task 3 ──→ Task 6 (documentation)
```

- Tasks 3, 4, 5 can run in parallel after Task 2 completes
- Task 6 runs after Task 3 (needs final behavior locked for accurate docs)
- Tasks 1 → 2 must be sequential

## Final Verification

After all tasks complete, run the full verification suite:

```bash
# TS contracts
bun test packages/contracts/src/envelope.test.ts

# Go contracts (normalize)
cd packages/envoy && go test ./internal/contracts/

# Go routing (match proof)
cd packages/envoy && go test ./internal/routing/

# Go store (subscription proof)
cd packages/envoy && go test ./internal/store/

# Full Go test suite
cd packages/envoy && go test ./...

# TS type check
bunx tsc --noEmit
```

All must pass with exit code 0.

---

## Testing Plan

### Setup
- `bun install` (if not already done)
- No running services needed — all tests are unit/contract tests

### Health Check
- `bun test packages/contracts/src/envelope.test.ts` — should pass with existing tests before any changes
- `cd packages/envoy && go test ./...` — should pass before any changes

### Verification Steps

1. **slackThreadSubject normalizes thread_ts**
   - Action: Run `bun test packages/contracts/src/envelope.test.ts`
   - Expected: `slackThreadSubject("T123", "C456", "1234567890.123456", "message")` returns `notifications.slack.T123.C456.thread.1234567890_123456.message`
   - Tool: bun test

2. **Thread reply produces normalized thread envelope**
   - Action: Run `cd packages/envoy && go test ./internal/contracts/ -run TestSlackEnvelopesThread -v`
   - Expected: 2 envelopes: `notifications.slack.T123.C123.message` + `notifications.slack.T123.C123.thread.1234567890_123456.message`
   - Tool: go test

3. **Thread mention produces .mention kind suffix**
   - Action: Run `cd packages/envoy && go test ./internal/contracts/ -run TestSlackEnvelopesThreadMention -v`
   - Expected: 2 envelopes with `.mention` kind on both channel and thread topics
   - Tool: go test

4. **No thread_ts = no thread envelope (ts fallback removed)**
   - Action: Run `cd packages/envoy && go test ./internal/contracts/ -run TestSlackEnvelopesNoThread -v`
   - Expected: 1 envelope only (channel-level), no thread envelope
   - Tool: go test

5. **Thread topic isolation (routing level)**
   - Action: Run `cd packages/envoy && go test ./internal/routing/ -run TestPerSlackThreadFiltering -v`
   - Expected: Thread subscription matches target thread only, rejects other threads and channel-level topics
   - Tool: go test

6. **Normalized ts is single NATS segment (included in routing test)**
   - Action: Run `cd packages/envoy && go test ./internal/routing/ -run TestPerSlackThreadFiltering -v`
   - Expected: `*` wildcard matches normalized `1234567890_123456` but NOT raw `1234567890.123456`
   - Tool: go test

7. **Subscription-level thread isolation**
   - Action: Run `cd packages/envoy && go test ./internal/store/ -run TestMatch_SlackThreadIsolation -v`
   - Expected: Thread A subscriber gets only thread A events, thread B subscriber gets only thread B events, channel subscriber gets all
   - Tool: go test

8. **Generated Go code is valid**
   - Action: Run `gofmt -l packages/envoy/internal/contracts/generated.go`
   - Expected: No output (file is correctly formatted)
   - Tool: gofmt

9. **Documentation contains expected patterns**
   - Action: Run `grep -c 'slackThreadSubject' packages/contracts/README.md && grep -c 'thread.<normalized_ts>.message' .opencode/skills/envoy/SKILL.md`
   - Expected: Both return ≥1 (patterns are present in both docs)
   - Tool: grep

### Tools Needed
- `bun test` for TypeScript contract tests
- `go test` for Go contract, routing, and store tests
- `gofmt` for generated code validation
- `bunx tsc --noEmit` for TypeScript type checking
- `grep` for documentation pattern validation
