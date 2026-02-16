# GitHub Labels Reference

## Adding Labels
gh issue edit $ISSUE_NUMBER --add-label "worker-done" -R $OWNER/$REPO

## Removing Labels  
gh issue edit $ISSUE_NUMBER --remove-label "worker-active" -R $OWNER/$REPO

## Signal Labels
- `worker-done` — Worker completed its mode
- `worker-active` — Worker is currently running
- `user-input-needed` — Worker blocked, needs human input
- `user-feedback-given` — Human provided feedback
- `needs-approval` — Plan/architecture needs human approval
- `human-approved` — Human approved the plan

## Key Difference from Linear
Labels are **additive**. Use `--add-label` to add and `--remove-label` to remove.
No need to fetch current labels first (unlike Linear's replace-all semantics).
