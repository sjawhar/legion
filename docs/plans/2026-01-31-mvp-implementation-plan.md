# Ralph Dev Swarm MVP Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Build the minimum system to prove the core autonomous development loop works: Daemon starts Controller, Controller polls Linear and spawns workers, workers implement code and self-terminate.

**Architecture:** A persistent Daemon process manages a tmux session. It starts an ephemeral Controller (Claude Code skill) that polls Linear for "In Progress" issues, spawns workers in separate tmux windows with jj workspaces, and merges completed work. Workers use the `ralph-dev-execute` skill and self-terminate via hooks when done.

**Tech Stack:** Bash (Daemon), Claude Code skills (Controller, Worker), tmux (process management), jj (version control), linear-cli (Linear API), Claude Code hooks (lifecycle automation)

---

## Prerequisites

Before starting, ensure these tools are installed and configured:

```bash
# Check prerequisites
which tmux && tmux -V    # tmux 3.x+
which jj && jj --version # jj 0.24+
which claude && claude --version # Claude Code
# linear-cli installed as Claude Code plugin
```

**Linear Setup:**
- Create a Linear project for testing
- Note the project ID (from URL: `linear.app/team/PROJECT/...`)
- Ensure `linear` CLI is authenticated (`linear auth`)

---

## Task 1: Create project structure

**Files:**
- Create: `~/.claude/skills/ralph-dev-controller/SKILL.md`
- Create: `~/.claude/skills/ralph-dev-execute/SKILL.md`
- Create: `~/swarm/bin/ralph-dev`
- Create: `~/swarm/.claude/hooks/hooks.json`
- Create: `~/swarm/.claude/settings.json`

**Step 1: Create skills directory structure**

```bash
mkdir -p ~/.claude/skills/ralph-dev-controller
mkdir -p ~/.claude/skills/ralph-dev-execute
```

**Step 2: Create swarm project directories**

```bash
mkdir -p ~/swarm/bin
mkdir -p ~/swarm/.claude/hooks
```

**Step 3: Verify directories exist**

Run: `ls -la ~/.claude/skills/ ~/swarm/bin ~/swarm/.claude/`
Expected: All directories present

**Step 4: Commit**

```bash
cd ~/swarm && jj describe -m "chore: create project structure for Ralph Dev Swarm MVP"
```

---

## Task 2: Implement the Daemon entry point

**Files:**
- Create: `~/swarm/bin/ralph-dev`

The Daemon is a bash script that:
1. Creates/attaches to a tmux session
2. Starts the Controller in a window
3. Runs a health loop checking heartbeat staleness

**Step 1: Write the Daemon script**

```bash
#!/usr/bin/env bash
#
# ralph-dev: Entry point for Ralph Dev Swarm
#
# Usage:
#   ralph-dev start PROJECT_ID   Start the swarm for a Linear project
#   ralph-dev stop PROJECT_ID    Stop the swarm
#   ralph-dev status PROJECT_ID  Check swarm health
#
set -euo pipefail

RALPH_DIR="${RALPH_DIR:-$HOME/.ralph}"
HEALTH_INTERVAL_SECONDS=180  # 3 minutes

usage() {
  cat <<EOF
Usage: ralph-dev <command> <project-id>

Commands:
  start   Start the swarm for a Linear project
  stop    Stop the swarm gracefully
  status  Check swarm health

Examples:
  ralph-dev start ENG
  ralph-dev stop ENG
  ralph-dev status ENG
EOF
}

# Derive session name from project ID
session_name() {
  echo "ralph-$1"
}

# Heartbeat file path
heartbeat_file() {
  echo "$RALPH_DIR/$1/heartbeat"
}

# Check if heartbeat is stale (older than HEALTH_INTERVAL_SECONDS)
is_heartbeat_stale() {
  local project_id="$1"
  local hb_file
  hb_file=$(heartbeat_file "$project_id")

  if [[ ! -f "$hb_file" ]]; then
    return 0  # No heartbeat = stale
  fi

  local now last_beat age
  now=$(date +%s)
  last_beat=$(stat -c %Y "$hb_file" 2>/dev/null || stat -f %m "$hb_file" 2>/dev/null)
  age=$((now - last_beat))

  [[ $age -gt $HEALTH_INTERVAL_SECONDS ]]
}

# Start the Controller in a tmux window
start_controller() {
  local project_id="$1"
  local session
  session=$(session_name "$project_id")

  # Create state directory
  mkdir -p "$RALPH_DIR/$project_id"

  # Start Claude with the controller skill
  tmux send-keys -t "$session:controller" \
    "cd ~/swarm && LINEAR_PROJECT_ID=$project_id claude -p 'Use the ralph-dev-controller skill. Project: $project_id'" Enter
}

# Health loop: check heartbeat, log staleness (MVP: no Supervisor spawn)
health_loop() {
  local project_id="$1"
  local session
  session=$(session_name "$project_id")

  while true; do
    sleep "$HEALTH_INTERVAL_SECONDS"

    # Check if tmux session still exists
    if ! tmux has-session -t "$session" 2>/dev/null; then
      echo "[$(date -Iseconds)] Session $session no longer exists, exiting health loop"
      break
    fi

    # Check heartbeat
    if is_heartbeat_stale "$project_id"; then
      echo "[$(date -Iseconds)] WARNING: Controller heartbeat stale for $project_id"
      # MVP: Just log, don't spawn Supervisor yet
      # TODO: spawn Supervisor to investigate
    else
      echo "[$(date -Iseconds)] Controller heartbeat OK for $project_id"
    fi
  done
}

cmd_start() {
  local project_id="$1"
  local session
  session=$(session_name "$project_id")

  # Check if already running
  if tmux has-session -t "$session" 2>/dev/null; then
    echo "Swarm already running for $project_id (session: $session)"
    echo "Use 'ralph-dev stop $project_id' to stop it first"
    exit 1
  fi

  echo "Starting Ralph Dev Swarm for project: $project_id"

  # Create tmux session with controller window
  tmux new-session -d -s "$session" -n controller

  echo "Created tmux session: $session"

  # Start the Controller
  start_controller "$project_id"

  echo "Started Controller in window: controller"
  echo ""
  echo "To attach: tmux attach -t $session"
  echo "To view:   tmux capture-pane -t $session:controller -p"
  echo ""

  # Start health loop in background
  health_loop "$project_id" &
  local health_pid=$!
  echo "Health loop started (PID: $health_pid)"

  # Save PID for stop command
  echo "$health_pid" > "$RALPH_DIR/$project_id/health.pid"
}

cmd_stop() {
  local project_id="$1"
  local session
  session=$(session_name "$project_id")

  echo "Stopping Ralph Dev Swarm for project: $project_id"

  # Kill health loop if running
  local pid_file="$RALPH_DIR/$project_id/health.pid"
  if [[ -f "$pid_file" ]]; then
    local health_pid
    health_pid=$(cat "$pid_file")
    if kill -0 "$health_pid" 2>/dev/null; then
      kill "$health_pid"
      echo "Stopped health loop (PID: $health_pid)"
    fi
    rm -f "$pid_file"
  fi

  # Kill tmux session
  if tmux has-session -t "$session" 2>/dev/null; then
    tmux kill-session -t "$session"
    echo "Killed tmux session: $session"
  else
    echo "No tmux session found: $session"
  fi
}

cmd_status() {
  local project_id="$1"
  local session
  session=$(session_name "$project_id")

  echo "Ralph Dev Swarm Status: $project_id"
  echo "========================="

  # Check tmux session
  if tmux has-session -t "$session" 2>/dev/null; then
    echo "tmux session: RUNNING"
    echo ""
    echo "Windows:"
    tmux list-windows -t "$session" -F '  #{window_name}: #{window_active}'
  else
    echo "tmux session: NOT RUNNING"
  fi

  echo ""

  # Check heartbeat
  local hb_file
  hb_file=$(heartbeat_file "$project_id")
  if [[ -f "$hb_file" ]]; then
    local now last_beat age
    now=$(date +%s)
    last_beat=$(stat -c %Y "$hb_file" 2>/dev/null || stat -f %m "$hb_file" 2>/dev/null)
    age=$((now - last_beat))
    echo "Heartbeat: ${age}s ago"
    if is_heartbeat_stale "$project_id"; then
      echo "  WARNING: Heartbeat is stale"
    fi
  else
    echo "Heartbeat: NO FILE"
  fi

  # Check health loop
  local pid_file="$RALPH_DIR/$project_id/health.pid"
  if [[ -f "$pid_file" ]]; then
    local health_pid
    health_pid=$(cat "$pid_file")
    if kill -0 "$health_pid" 2>/dev/null; then
      echo "Health loop: RUNNING (PID: $health_pid)"
    else
      echo "Health loop: DEAD (stale PID file)"
    fi
  else
    echo "Health loop: NOT RUNNING"
  fi
}

# Main
case "${1:-}" in
  start)
    [[ -z "${2:-}" ]] && { echo "Error: PROJECT_ID required"; usage; exit 1; }
    cmd_start "$2"
    ;;
  stop)
    [[ -z "${2:-}" ]] && { echo "Error: PROJECT_ID required"; usage; exit 1; }
    cmd_stop "$2"
    ;;
  status)
    [[ -z "${2:-}" ]] && { echo "Error: PROJECT_ID required"; usage; exit 1; }
    cmd_status "$2"
    ;;
  -h|--help|help)
    usage
    ;;
  *)
    usage
    exit 1
    ;;
esac
```

**Step 2: Make executable and add to PATH**

Run: `chmod +x ~/swarm/bin/ralph-dev`

**Step 3: Test the script parses correctly**

Run: `~/swarm/bin/ralph-dev --help`
Expected: Usage message displayed

**Step 4: Test start/stop without Linear (dry run)**

Run: `~/swarm/bin/ralph-dev start TEST && sleep 2 && ~/swarm/bin/ralph-dev status TEST && ~/swarm/bin/ralph-dev stop TEST`
Expected: Session created, status shown, session killed (Controller will fail without skill, that's OK)

**Step 5: Commit**

```bash
cd ~/swarm && jj describe -m "feat: add ralph-dev daemon entry point"
```

---

## Task 3: Implement the Controller skill

**Files:**
- Create: `~/.claude/skills/ralph-dev-controller/SKILL.md`

The Controller skill:
1. Polls Linear for "In Progress" issues assigned to the project
2. Checks which issues already have workers (tmux windows)
3. Spawns workers for issues without workers
4. Detects completed workers and merges their workspaces
5. Writes heartbeat file
6. Exits (Daemon restarts via Ralph loop pattern)

**Step 1: Write the Controller skill**

```markdown
---
name: ralph-dev-controller
description: Poll Linear, spawn workers, merge completed workspaces
---

# Ralph Dev Controller

You are the Controller for Ralph Dev Swarm. Your job is to coordinate workers implementing Linear issues.

## Environment

Required environment variable: `LINEAR_PROJECT_ID` - the Linear project to poll.

State directory: `~/.ralph/$LINEAR_PROJECT_ID/`

## Iteration Steps

**Execute these steps in order, then exit:**

### Step 1: Get Linear state

Query Linear for issues assigned to this project that are "In Progress":

```bash
linear issue list --project "$LINEAR_PROJECT_ID" --status "In Progress" --json
```

Parse the output to get issue identifiers (e.g., `ENG-21`, `ENG-22`).

### Step 2: Get tmux state

List existing worker windows:

```bash
tmux list-windows -t "ralph-$LINEAR_PROJECT_ID" -F '#{window_name}' 2>/dev/null | grep -v controller || true
```

### Step 3: Identify work

Compare the two lists:

- **Orphaned issues**: In Progress in Linear, but no tmux window → spawn worker
- **Zombie windows**: tmux window exists, but issue not In Progress → clean up
- **Active workers**: Both exist → leave alone

### Step 4: Spawn workers for orphaned issues

For each orphaned issue:

```bash
ISSUE_ID="ENG-XX"  # The issue identifier
SESSION="ralph-$LINEAR_PROJECT_ID"

# Create jj workspace
cd ~/swarm && jj workspace add "$ISSUE_ID" --directory "$HOME/swarm/$ISSUE_ID"

# Create tmux window
tmux new-window -t "$SESSION" -n "$ISSUE_ID" -d

# Start Claude worker
tmux send-keys -t "$SESSION:$ISSUE_ID" "cd ~/swarm/$ISSUE_ID && LINEAR_ISSUE_ID=$ISSUE_ID claude -p 'Use the ralph-dev-execute skill for issue $ISSUE_ID'" Enter
```

### Step 5: Clean up zombies

For each zombie window (tmux window but no matching "In Progress" issue):

```bash
# Check if workspace exists and has uncommitted changes
if jj workspace list | grep -q "$ISSUE_ID"; then
  # Merge workspace back to main first
  cd ~/swarm && jj new "$ISSUE_ID"@ @
  jj workspace forget "$ISSUE_ID"
fi

# Kill the window
tmux kill-window -t "ralph-$LINEAR_PROJECT_ID:$ISSUE_ID"
```

### Step 6: Merge completed workspaces

For each issue that was "In Progress" but worker window is gone (completed naturally):

```bash
ISSUE_ID="ENG-XX"

# Check if workspace still exists
if jj workspace list | grep -q "$ISSUE_ID"; then
  cd ~/swarm

  # Merge the workspace's changes into main
  jj new "$ISSUE_ID"@ @

  # Clean up the workspace
  jj workspace forget "$ISSUE_ID"
  rm -rf "$HOME/swarm/$ISSUE_ID"

  echo "Merged workspace for $ISSUE_ID"
fi
```

### Step 7: Write heartbeat

```bash
touch ~/.ralph/$LINEAR_PROJECT_ID/heartbeat
```

### Step 8: Exit

After completing all steps, simply end your response. The Daemon will restart you for the next iteration.

## Important Notes

- **One iteration only**: Complete steps 1-8, then stop. Don't loop.
- **Idempotent**: Running twice should be safe.
- **Non-blocking**: Don't wait for workers to complete. Just spawn and move on.
- **Log everything**: Print what you're doing so the user can follow along.
```

**Step 2: Verify skill file exists and is valid markdown**

Run: `cat ~/.claude/skills/ralph-dev-controller/SKILL.md | head -20`
Expected: Frontmatter and content visible

**Step 3: Test skill loads in Claude Code**

Run: `claude -p "What does the ralph-dev-controller skill do? Just summarize in one sentence, don't execute it."`
Expected: Claude describes the skill's purpose

**Step 4: Commit**

```bash
cd ~/swarm && jj describe -m "feat: add ralph-dev-controller skill"
```

---

## Task 4: Implement the Worker skill

**Files:**
- Create: `~/.claude/skills/ralph-dev-execute/SKILL.md`

The Worker skill:
1. Reads the assigned Linear issue
2. Implements the requested changes
3. Runs tests
4. Pushes branch and creates/updates PR
5. Updates Linear issue status
6. Self-terminates (cleanup via hooks)

**Step 1: Write the Worker skill**

```markdown
---
name: ralph-dev-execute
description: Implement a Linear issue autonomously
---

# Ralph Dev Execute

You are a Worker implementing a single Linear issue. Focus on completing the task, then cleanly exit.

## Environment

Required environment variable: `LINEAR_ISSUE_ID` - the issue you're implementing (e.g., `ENG-21`).

You are running in a jj workspace directory: `~/swarm/$LINEAR_ISSUE_ID/`

## Workflow

### Step 1: Understand the task

Fetch the issue details:

```bash
linear issue view "$LINEAR_ISSUE_ID"
```

Read the issue title, description, and any comments carefully.

### Step 2: Implement the solution

Follow these principles:
- **TDD**: Write tests first when appropriate
- **Small commits**: Use `jj` to snapshot work frequently
- **Ask for help**: If truly blocked, add `user-input-needed` label and comment on the issue

Implementation loop:
1. Understand what needs to change
2. Write/modify code
3. Run tests: `jj status` (auto-snapshots), then run project's test command
4. If tests fail, fix and repeat
5. If tests pass, continue to next step

### Step 3: Describe your changes

Update the jj commit description with what you did:

```bash
jj describe -m "$LINEAR_ISSUE_ID: Brief description of changes"
```

### Step 4: Push and create PR

```bash
# Push the branch
jj git push --named "$LINEAR_ISSUE_ID"=@

# Create PR (or update if exists)
gh pr create --title "$LINEAR_ISSUE_ID: Title" --body "Closes $LINEAR_ISSUE_ID" --head "$LINEAR_ISSUE_ID" || \
  gh pr edit --title "$LINEAR_ISSUE_ID: Title" --body "Closes $LINEAR_ISSUE_ID"
```

### Step 5: Update Linear

Mark the issue as ready for review:

```bash
linear issue update "$LINEAR_ISSUE_ID" --status "In Review"
```

Add a comment with the PR link:

```bash
PR_URL=$(gh pr view --json url -q '.url')
linear issue comment "$LINEAR_ISSUE_ID" "PR ready for review: $PR_URL"
```

### Step 6: Exit

After completing all steps, end your response. Hooks will handle cleanup.

## Self-Termination

When you finish (either successfully or due to a blocker), just stop responding. The hooks configured in the project will:
1. Update Linear status
2. Clean up the jj workspace
3. Close the tmux window

## Handling Blockers

If you encounter a blocker you cannot resolve:

1. Add label: `linear issue label "$LINEAR_ISSUE_ID" --add "user-input-needed"`
2. Comment explaining the blocker: `linear issue comment "$LINEAR_ISSUE_ID" "Blocked: [explanation]"`
3. Exit and let the Controller handle it next iteration

## Important Notes

- **One issue only**: You implement exactly `$LINEAR_ISSUE_ID`, nothing else.
- **Don't loop**: Complete the workflow once, then exit.
- **Workspace isolation**: Your changes are in a separate jj workspace. They won't conflict with other workers.
```

**Step 2: Verify skill file exists**

Run: `cat ~/.claude/skills/ralph-dev-execute/SKILL.md | head -20`
Expected: Frontmatter and content visible

**Step 3: Test skill loads in Claude Code**

Run: `claude -p "What does the ralph-dev-execute skill do? Just summarize in one sentence, don't execute it."`
Expected: Claude describes the skill's purpose

**Step 4: Commit**

```bash
cd ~/swarm && jj describe -m "feat: add ralph-dev-execute worker skill"
```

---

## Task 5: Implement Claude Code hooks

**Files:**
- Create: `~/swarm/.claude/hooks/session-start.sh`
- Create: `~/swarm/.claude/hooks/post-tool-use.sh`
- Create: `~/swarm/.claude/hooks/stop.sh`
- Create: `~/swarm/.claude/settings.json`

Hooks automate worker lifecycle:
- **SessionStart**: Inject Linear issue context
- **PostToolUse**: Snapshot working copy after edits
- **Stop**: Self-termination check and cleanup

**Step 1: Write SessionStart hook**

```bash
#!/usr/bin/env bash
# session-start.sh: Inject Linear issue context into worker sessions
set -euo pipefail

# Read hook input from stdin
INPUT=$(cat)

# Check if this is a worker session (has LINEAR_ISSUE_ID)
if [[ -z "${LINEAR_ISSUE_ID:-}" ]]; then
  exit 0  # Not a worker, nothing to inject
fi

# Fetch issue details and output as additional context
ISSUE_DETAILS=$(linear issue view "$LINEAR_ISSUE_ID" 2>/dev/null || echo "Failed to fetch issue $LINEAR_ISSUE_ID")

cat <<EOF
{
  "hookSpecificOutput": {
    "hookEventName": "SessionStart",
    "additionalContext": "You are implementing Linear issue $LINEAR_ISSUE_ID.\n\nIssue Details:\n$ISSUE_DETAILS"
  }
}
EOF
```

**Step 2: Write PostToolUse hook**

```bash
#!/usr/bin/env bash
# post-tool-use.sh: Auto-snapshot working copy after file changes
set -euo pipefail

# Read hook input
INPUT=$(cat)
TOOL_NAME=$(echo "$INPUT" | jq -r '.tool_name // empty')

# Only run after Edit or Write tools
case "$TOOL_NAME" in
  Edit|Write)
    # jj status triggers automatic working copy snapshot
    jj status >/dev/null 2>&1 || true
    ;;
esac

exit 0
```

**Step 3: Write Stop hook**

```bash
#!/usr/bin/env bash
# stop.sh: Self-termination check for workers
set -euo pipefail

# Read hook input
INPUT=$(cat)

# Check if this is a worker session
if [[ -z "${LINEAR_ISSUE_ID:-}" ]]; then
  exit 0  # Not a worker, let it stop normally
fi

# Check if stop hook is already active (prevent infinite loop)
STOP_HOOK_ACTIVE=$(echo "$INPUT" | jq -r '.stop_hook_active // false')
if [[ "$STOP_HOOK_ACTIVE" == "true" ]]; then
  # Already in stop hook, allow stop
  exit 0
fi

# For MVP: always allow stop (cleanup happens externally via Controller)
# Future: check if task is complete before allowing stop

exit 0
```

**Step 4: Make hooks executable**

```bash
chmod +x ~/swarm/.claude/hooks/*.sh
```

**Step 5: Write settings.json to register hooks**

```json
{
  "hooks": {
    "SessionStart": [
      {
        "matcher": "startup",
        "hooks": [
          {
            "type": "command",
            "command": "\"$CLAUDE_PROJECT_DIR\"/.claude/hooks/session-start.sh"
          }
        ]
      }
    ],
    "PostToolUse": [
      {
        "matcher": "Edit|Write",
        "hooks": [
          {
            "type": "command",
            "command": "\"$CLAUDE_PROJECT_DIR\"/.claude/hooks/post-tool-use.sh"
          }
        ]
      }
    ],
    "Stop": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "\"$CLAUDE_PROJECT_DIR\"/.claude/hooks/stop.sh"
          }
        ]
      }
    ]
  }
}
```

**Step 6: Verify hooks configuration**

Run: `cat ~/swarm/.claude/settings.json | jq .`
Expected: Valid JSON with hooks configured

**Step 7: Test hooks load**

Run: `cd ~/swarm && claude /hooks`
Expected: Shows configured hooks from settings.json

**Step 8: Commit**

```bash
cd ~/swarm && jj describe -m "feat: add Claude Code hooks for worker lifecycle"
```

---

## Task 6: Integration test - Manual Controller iteration

**Files:** None (testing existing code)

Test the Controller skill manually to verify it can:
1. Query Linear
2. Detect existing tmux windows
3. Spawn a worker

**Step 1: Create a test issue in Linear**

Manually create an issue in your Linear project:
- Title: "Test: Create hello.txt"
- Description: "Create a file called hello.txt with the content 'Hello, Ralph!'"
- Status: "In Progress"

Note the issue ID (e.g., `ENG-123`).

**Step 2: Start the Daemon**

```bash
~/swarm/bin/ralph-dev start YOUR_PROJECT_ID
```

Replace `YOUR_PROJECT_ID` with your actual Linear project identifier.

**Step 3: Observe Controller behavior**

```bash
# Attach to see what's happening
tmux attach -t ralph-YOUR_PROJECT_ID
```

Watch the Controller:
1. Query Linear for In Progress issues
2. Find the test issue
3. Create jj workspace
4. Spawn worker in new tmux window

**Step 4: Observe Worker behavior**

Switch to the worker window (Ctrl+B, then window number) and watch it:
1. Read the issue
2. Create hello.txt
3. Push branch
4. Create PR
5. Update Linear status

**Step 5: Verify results**

```bash
# Check the file was created
cat ~/swarm/ENG-123/hello.txt  # Should contain "Hello, Ralph!"

# Check PR was created
gh pr list

# Check Linear status changed
linear issue view ENG-123
```

**Step 6: Clean up**

```bash
~/swarm/bin/ralph-dev stop YOUR_PROJECT_ID
```

**Step 7: Document results**

Note any issues encountered for fixing in subsequent iterations.

---

## Task 7: End-to-end test with full loop

**Files:** None (testing existing code)

Test the complete loop: Daemon → Controller → Worker → Completion → Merge

**Step 1: Create fresh test issue**

In Linear, create:
- Title: "Test: Add greeting function"
- Description: "Add a function `greet(name)` in `lib/greeting.py` that returns 'Hello, {name}!'"
- Status: "In Progress"

**Step 2: Start Daemon**

```bash
~/swarm/bin/ralph-dev start YOUR_PROJECT_ID
```

**Step 3: Wait for completion**

Monitor progress:
```bash
# Watch tmux windows
watch -n5 "tmux list-windows -t ralph-YOUR_PROJECT_ID"

# Check Linear status
watch -n10 "linear issue view ENG-XXX --json | jq '.status'"
```

**Step 4: Verify merge happened**

After worker completes and window closes:
```bash
# Check Controller merged the workspace
jj log --limit 5

# Verify the file exists in main workspace
cat ~/swarm/lib/greeting.py
```

**Step 5: Verify PR created**

```bash
gh pr list
gh pr view ENG-XXX
```

**Step 6: Stop and clean up**

```bash
~/swarm/bin/ralph-dev stop YOUR_PROJECT_ID
```

---

## Done Criteria

The MVP is complete when:

1. **Daemon works**: `ralph-dev start/stop/status` commands function correctly
2. **Controller polls**: Controller queries Linear and detects In Progress issues
3. **Worker spawns**: Workers are created in separate tmux windows with jj workspaces
4. **Worker implements**: Workers read issues, make changes, run tests, create PRs
5. **Worker self-terminates**: Workers exit after completing their task
6. **Workspace merges**: Controller merges completed workspaces back to main
7. **Heartbeat updates**: Controller writes heartbeat file each iteration
8. **Health loop runs**: Daemon detects stale heartbeats (logs warning for MVP)

---

## Known Risks and Mitigations

| Risk | Mitigation |
|------|------------|
| linear-cli not installed | Check prerequisites; install as Claude Code plugin |
| jj workspace conflicts | Each worker gets isolated workspace; Controller merges serially |
| Worker hangs | MVP: manual intervention; Future: Supervisor detects and kills |
| Linear API rate limits | Controller polls once per iteration (~1 min); well under limits |
| tmux session naming conflicts | Use `ralph-$PROJECT_ID` pattern to isolate |

---

## Out of Scope (Future Work)

- Supervisor reasoning and intervention
- Other worker skills (plan, review, resolve, research)
- Parallel conflict resolution
- Pattern learning
- Local-first state complement
- Multiple simultaneous projects
