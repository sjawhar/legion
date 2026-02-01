---
title: "Navigating AskUserQuestion Dialogs in Claude Code via tmux"
date: 2026-02-01
category: integration-patterns
tags:
  - tmux
  - claude-code
  - remote-control
  - askuserquestion
  - keyboard-navigation
  - automation
  - cli-interaction
module: claude-code-cli
component: interactive-prompts
symptoms:
  - "tmux send-keys not selecting option"
  - "claude code remote control"
  - "navigate AskUserQuestion programmatically"
  - "tmux arrow keys not working"
  - "claude code tmux automation"
  - "select non-default option claude code"
  - "Down key tmux claude"
slug: tmux-askuserquestion-navigation
---

# Navigating AskUserQuestion Dialogs in Claude Code via tmux

## Problem

When controlling a Claude Code session remotely via tmux, AskUserQuestion prompts present multiple options. Simply sending Enter accepts the default (first option). To select a different option, you need to navigate with arrow keys—but sending keys too quickly causes missed inputs.

## Symptoms

- `tmux send-keys Down` doesn't move the selection
- Always selects the first option regardless of navigation attempts
- Arrow keys appear to be ignored
- Inconsistent behavior when automating Claude Code

## Root Cause

When Claude Code presents an `AskUserQuestion` dialog with multiple options, it uses an interactive selection interface where:

1. The **first option is selected by default**
2. Navigation requires **arrow keys** (Up/Down) to move the selection cursor
3. **Enter key** confirms the currently highlighted selection

The initial attempts failed because:
- **Sending keys too quickly**: tmux can send keys faster than the terminal UI can process them, causing missed inputs
- **No navigation**: Just pressing Enter always selects the default (first) option regardless of intent

## Solution

### Step 1: Identify the Target Option Position

Count how many positions down from the first option your target is:

| Target Option | Down Presses Needed |
|---------------|---------------------|
| Option 1 (default) | 0 |
| Option 2 | 1 |
| Option 3 | 2 |
| Option 4 | 3 |

### Step 2: Send Navigation Keys with Delays

Use `sleep 0.2` between each keypress to ensure reliable processing:

```bash
# Example: Select option 4 (3 Downs from option 1)
tmux send-keys -t <session>:<window>.<pane> Down && \
sleep 0.2 && \
tmux send-keys -t <session>:<window>.<pane> Down && \
sleep 0.2 && \
tmux send-keys -t <session>:<window>.<pane> Down && \
sleep 0.2 && \
tmux send-keys -t <session>:<window>.<pane> Enter
```

### Step 3: Verify the Selection

```bash
# Wait for Claude to process, then capture output
sleep 2 && tmux capture-pane -t dev2:2.2 -p -S -15
```

Verify that:
- The dialog has closed
- Claude shows the correct selected option
- The output reflects the correct choice

## Timing Considerations

| Delay Duration | Reliability | Use Case |
|----------------|-------------|----------|
| 0.1s | Sometimes fails | Fast local systems |
| 0.2s | Reliable | Recommended default |
| 0.3s | Very reliable | Remote/slow systems |
| 0.5s | Overkill | Only if 0.3s fails |

## Reusable Shell Function

```bash
#!/usr/bin/env bash
# select_dialog_option - Navigate and select an option in a Claude Code dialog
#
# Usage: select_dialog_option <session> <option_number> [delay_ms]

select_dialog_option() {
    local session="$1"
    local option_number="$2"
    local delay_ms="${3:-200}"
    local delay_sec

    delay_sec=$(echo "scale=3; $delay_ms / 1000" | bc)

    # Validate inputs
    if [ -z "$session" ] || [ -z "$option_number" ]; then
        echo "Error: session and option_number required" >&2
        return 1
    fi

    # Calculate downs needed (0-indexed navigation)
    local downs_needed=$((option_number - 1))

    # Send Down keys with delays
    for ((i = 0; i < downs_needed; i++)); do
        tmux send-keys -t "$session" Down
        sleep "$delay_sec"
    done

    # Small delay before Enter
    sleep "$delay_sec"

    # Confirm selection
    tmux send-keys -t "$session" Enter

    return 0
}

# Example usage:
# select_dialog_option "dev2:2.2" 4      # Select option 4 (Purple)
# select_dialog_option "claude:0" 2 150  # Select option 2 with 150ms delay
```

## Prevention Checklist

- [ ] **Delay between keys**: 200ms minimum between arrow key presses
- [ ] **Verify dialog present**: Check pane output shows dialog before interacting
- [ ] **Calculate navigation correctly**: N-1 downs to reach option N
- [ ] **Verify selection**: Capture pane after Enter to confirm correct option
- [ ] **Use explicit targets**: Always specify session:window.pane format

## Related

- Claude Code AskUserQuestion tool documentation
- tmux send-keys man page
