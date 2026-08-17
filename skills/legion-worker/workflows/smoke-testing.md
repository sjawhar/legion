# Smoke Testing Workflow

Run real integration/smoke tests against OpenCode plugin hooks using standalone serve instances.

## Prerequisites
- Plugin code built in your workspace: `cd packages/opencode-plugin && bun run build`
- OpenCode binary available (set `OC_BIN` below to the correct path)
- A free port for your standalone serve (e.g., 14000-14999)

## Step 1: Start Standalone Serve (tmux — NON-BLOCKING)

Set up variables and start the serve in a tmux session so it does not block your bash:

```bash
PORT=14000
OC_BIN="$HOME/opencode/default/packages/opencode/dist/opencode-linux-x64/bin/opencode"
tmux new-session -d -s smoke-test
tmux send-keys -t smoke-test "PATH=$(dirname $OC_BIN):\$PATH opencode serve --port $PORT" Enter
```

**CRITICAL:** Do NOT run the serve in your normal bash session. It will block all subsequent commands.

## Step 2: Wait for Serve Ready

Poll the health endpoint with a bounded timeout (30s). If the serve fails to start, check tmux logs:

```bash
for i in $(seq 1 30); do
  curl -sf http://localhost:$PORT/global/health > /dev/null && break
  if [ "$i" -eq 30 ]; then
    echo "FATAL: serve not ready after 30s"
    echo "Check serve logs: tmux capture-pane -t smoke-test -p"
    exit 1
  fi
  sleep 1
done
curl -s http://localhost:$PORT/global/health
```

## Step 3: Run Smoke Test

Create a test workspace and session, then trigger the hook condition:

```bash
mkdir -p /tmp/smoke-test
curl -s -X POST "http://localhost:$PORT/session?directory=/tmp/smoke-test" \
  -H "Content-Type: application/json" \
  -d "{\"id\": \"ses_smoke\"}"

curl -s -X POST "http://localhost:$PORT/session/ses_smoke/prompt_async?directory=/tmp/smoke-test" \
  -H "Content-Type: application/json" \
  -d "{\"parts\": [{\"type\": \"text\", \"text\": \"<prompt that triggers your hook>\"}]}"

# Wait for the AI to process. Adjust sleep duration based on hook complexity:
# - Simple hooks (file read, config check): 10-15s
# - Medium hooks (API calls, tool use): 20-30s
# - Complex hooks (multi-step workflows): 45-60s
sleep 30

curl -s "http://localhost:$PORT/session/ses_smoke/message?directory=/tmp/smoke-test&limit=10"
```

### Step 3.5: Verify Results

After reading messages, check the output for:

1. **Assistant response present** — at least one message with `role: "assistant"` exists
2. **No error messages** — no `error` fields or stack traces in the response
3. **Hook-specific behavior triggered** — the response shows evidence that your hook fired (e.g., tool calls made, expected output produced, files created/modified)

If any check fails, proceed to Step 3.6 to retry with the existing session.

### Step 3.6: Retry with Session Continuity

**Resume the existing session** for retries — do NOT create a new session or dispatch a fresh worker. Fresh sessions lose all debugging context.

```bash
# Adjust prompt based on what failed and retry on the SAME session
curl -s -X POST "http://localhost:$PORT/session/ses_smoke/prompt_async?directory=/tmp/smoke-test" \
  -H "Content-Type: application/json" \
  -d "{\"parts\": [{\"type\": \"text\", \"text\": \"<adjusted prompt based on previous failure>\"}]}"

sleep 30

curl -s "http://localhost:$PORT/session/ses_smoke/message?directory=/tmp/smoke-test&limit=10"
```

Repeat until the hook behavior is confirmed, then proceed to Step 4.

## Step 4: Cleanup

```bash
tmux kill-session -t smoke-test 2>/dev/null || true
rm -rf /tmp/smoke-test
```

## Anti-Patterns (DO NOT DO)

| Anti-Pattern | Why It Fails |
|---|---|
| Use shared serve (port 13381) | Does not have your plugin changes. Must restart to load, which kills all workers. |
| Run serve in normal bash | Blocks all subsequent commands. Worker appears hung. |
| Create nested sessions on shared serve | Circular deadlock — worker runs ON the serve it is testing. |
| Dispatch fresh workers for retries | Loses all debugging context. Resume existing sessions instead. |
| Synchronous wait for AI responses | Use prompt_async + sleep + read messages pattern. |
