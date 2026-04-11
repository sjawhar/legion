# Repository Configuration (.legion/config.yml)

Workers load repository-specific configuration from `.legion/config.yml` at the workspace root. This file allows teams to customize Legion behavior per repository without modifying workflows.

## Schema Overview

The config file is YAML with the following top-level sections:

```yaml
merge:
  require_smoke_test: boolean
  require_reporter_approval: boolean
  auto_merge_allowed: boolean

testing:
  require_specific_task: boolean
  require_taiga_evidence: boolean

notifications:
  slack_channel: string
  ping_reporter_on_pr: boolean

skills:
  required:
    - skill_name_1
    - skill_name_2

phases:
  architect:
    # Phase-specific overrides (same keys as top-level)
  plan:
    # Phase-specific overrides
  implement:
    # Phase-specific overrides
  test:
    # Phase-specific overrides
  review:
    # Phase-specific overrides
```

## Field Reference

### merge

Controls merge behavior and approval gates.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `require_smoke_test` | boolean | `false` | If `true`, merge workflow requires evidence of smoke testing before auto-merge |
| `require_reporter_approval` | boolean | `false` | If `true`, merge workflow requires explicit reporter approval before merging |
| `auto_merge_allowed` | boolean | `false` | If `true`, merge workflow may auto-merge PRs that pass all gates |

### testing

Controls test requirements and evidence gates.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `require_specific_task` | boolean | `false` | If `true`, test workflow requires evidence of specific task testing (not just unit tests) |
| `require_taiga_evidence` | boolean | `false` | If `true`, test workflow requires Taiga evidence (e.g., screenshots, logs) attached to the issue |

### notifications

Controls notifications and pings.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `slack_channel` | string | `null` | Slack channel ID (e.g., `#eng-legion`) where implementation status updates are posted. If set and `slack-bot` skill is available, implement workflow posts PR URL and CI state |
| `ping_reporter_on_pr` | boolean | `false` | If `true`, notify the issue reporter when a PR is created |

### skills

Specifies skills that should be invoked for all phases.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `required` | string[] | `[]` | List of skill names to invoke in addition to plan handoff `requiredSkills` and independently discovered skills. Additive with other skill sources |

### phases

Phase-specific overrides. Each phase (`architect`, `plan`, `implement`, `test`, `review`) can override any top-level key.

**Merge behavior:** Phase-specific values override top-level values. For example:

```yaml
merge:
  require_reporter_approval: false

phases:
  implement:
    merge:
      require_reporter_approval: true  # Override for implement phase only
```

## Recognized Keys

Workers recognize the following keys (case-sensitive):

- `merge.require_smoke_test`
- `merge.require_reporter_approval`
- `merge.auto_merge_allowed`
- `testing.require_specific_task`
- `testing.require_taiga_evidence`
- `notifications.slack_channel`
- `notifications.ping_reporter_on_pr`
- `skills.required`
- `phases.<mode>.*` (any of the above keys under a phase)

Unknown keys are silently ignored. Malformed YAML causes the config to be skipped (fallback to defaults).

## Worker Behavior

### Loading

Each workflow loads config at startup:

```bash
if [ -f .legion/config.yml ]; then cat .legion/config.yml; fi
```

If the file is missing or malformed, workers proceed with defaults (no error).

### Parsing

Workers parse recognized keys and echo them for auditability:

```
Repo config constraints from .legion/config.yml:
- merge.require_reporter_approval: true
- testing.require_specific_task: true
- skills.required: [test-driven-development, verification-before-completion]
```

### Application

Config values are applied to shape workflow behavior:

- **Plan phase:** Config constraints inform the plan preamble (e.g., "reporter approval required before merge")
- **Implement phase:** Config-required skills are invoked additively with plan handoff skills
- **Test phase:** Config gates (e.g., `require_specific_task`) are enforced
- **Review phase:** Config-required skills are invoked additively
- **Merge phase:** Config gates (e.g., `require_reporter_approval`) are enforced

## Example Configuration

### Minimal (all defaults)

```yaml
# Empty file or omitted entirely
# All defaults apply
```

### Strict Testing

```yaml
testing:
  require_specific_task: true
  require_taiga_evidence: true

skills:
  required:
    - test-driven-development
    - verification-before-completion
```

### Slack Notifications + Approval Gate

```yaml
notifications:
  slack_channel: "#eng-legion"
  ping_reporter_on_pr: true

merge:
  require_reporter_approval: true
  require_smoke_test: true
```

### Phase-Specific Overrides

```yaml
# Default: no approval required
merge:
  require_reporter_approval: false

# But for implement phase, require approval
phases:
  implement:
    merge:
      require_reporter_approval: true

# And require specific task testing only in test phase
phases:
  test:
    testing:
      require_specific_task: true
```

### Full Example

```yaml
merge:
  require_smoke_test: true
  require_reporter_approval: false
  auto_merge_allowed: false

testing:
  require_specific_task: true
  require_taiga_evidence: false

notifications:
  slack_channel: "#eng-legion"
  ping_reporter_on_pr: true

skills:
  required:
    - test-driven-development
    - verification-before-completion

phases:
  plan:
    skills:
      required:
        - brainstorming
  implement:
    merge:
      require_reporter_approval: true
  test:
    testing:
      require_taiga_evidence: true
```

## Defaults

If a key is not specified in `.legion/config.yml`, the following defaults apply:

```yaml
merge:
  require_smoke_test: false
  require_reporter_approval: false
  auto_merge_allowed: false

testing:
  require_specific_task: false
  require_taiga_evidence: false

notifications:
  slack_channel: null
  ping_reporter_on_pr: false

skills:
  required: []
```

## Notes

- **Additive skills:** Config-required skills are additive with plan handoff `requiredSkills` and independently discovered skills. All sources are combined.
- **Phase overrides:** Phase-specific values completely override top-level values for that phase. Partial overrides are not merged.
- **Missing file:** If `.legion/config.yml` is missing or malformed, workers proceed with defaults (no error).
- **Auditability:** Workers echo recognized keys and effective values at the start of each phase for transparency.
