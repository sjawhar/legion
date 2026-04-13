# Smoke Testing Workflow

Run real integration/smoke tests against OpenCode plugin hooks using standalone serve instances.

## Prerequisites
- Plugin code built in your workspace: `cd packages/opencode-plugin && bun run build`
- OpenCode binary available at `~/opencode/default/packages/opencode/dist/opencode-linux-x64/bin/opencode`
- A free port for your standalone serve (e.g., 14000-14999)

## Step 1: Start Standalone Serve (tmux — NON-BLOCKING)

Start the serve in a tmux session so it does not block your bash:

```bash
PORT=14000
tmux new-session -d -s smoke-test
tmux send-keys -t smoke-test "PATH=$HOME/opencode/default/packages/opencode/dist/opencode-linux-x64/bin:\$PATH opencode serve --port $PORT" Enter
```

**CRITICAL:** Do NOT run the serve in your normal bash session. It will block all subsequent commands.

## Step 2: Wait for Serve Ready

Poll the health endpoint until the serve is up:

```bash
until curl -sf http://localhost:$PORT/global/health > /dev/null; do sleep 1; done
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

sleep 30

curl -s "http://localhost:$PORT/session/ses_smoke/message?directory=/tmp/smoke-test&limit=10"
```

## Step 4: Cleanup

```bash
tmux kill-session -t smoke-test
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
