---
title: Refactoring monolithic state module to async package architecture
created: 2026-02-01
category: architecture-patterns
tags:
  - async
  - anyio
  - aiofiles
  - graphql
  - batch-queries
  - caching
  - alru-cache
  - dependency-injection
  - protocol
  - testability
  - n-plus-one
  - refactoring
  - modular-architecture
module: state
symptoms:
  - slow performance from sequential I/O operations
  - multiple gh CLI calls causing N+1 query problem
  - synchronous file I/O blocking execution
  - repeated tmux session queries without caching
  - difficult to write unit tests due to tight coupling
  - mixed concerns making code hard to maintain
related_issues: []
---

# Refactoring Sync Module to Async Package Architecture

## Problem

A monolithic `state.py` module suffered from multiple architectural issues:

- **Sequential I/O**: All external calls (tmux, GitHub, file reads) executed one after another
- **N+1 GitHub queries**: Individual `gh` CLI calls for each PR's labels instead of batching
- **Synchronous file reads**: Blocking file I/O for session file checks
- **No caching**: Repeated tmux session lookups without memoization
- **Mixed concerns**: Data types, fetching logic, decision logic, and CLI all in one file

## Root Cause

The original design treated the module as a simple script rather than a system component. As requirements grew (checking multiple PRs, reading session files, detecting blocked workers), the sequential approach accumulated latency. Each new data source added another blocking I/O call, and the lack of separation made testing individual components impossible without mocking the entire module.

## Solution

Refactored `state.py` into a `state/` package with clear separation of concerns and parallel async I/O.

### Package Structure

```
src/legion/state/
├── __init__.py     # Public API exports
├── __main__.py     # Module execution entry
├── types.py        # Dataclasses, TypedDicts, constants
├── fetch.py        # All async data fetching
├── decision.py     # Pure decision logic (no I/O)
└── cli.py          # CLI entry point
```

### Parallel I/O with Task Groups

All independent I/O operations run concurrently using `anyio.create_task_group()`:

```python
async with anyio.create_task_group() as tg:
    tg.start_soon(fetch_workers)      # tmux session list
    tg.start_soon(fetch_pr_draft_status)  # single GraphQL query

# Phase 2: Check blocked status (depends on live workers)
if session_dir and live_worker_issues:
    async with anyio.create_task_group() as tg:
        for issue in live_worker_issues:
            tg.start_soon(check_blocked, issue)
```

### GraphQL Batch Query

Replaced N individual `gh pr view` calls with a single GraphQL query using aliases:

```python
async def get_pr_draft_status_batch(
    pr_refs: dict[str, GitHubPRRef],
    *,
    runner: CommandRunner = default_runner,
) -> dict[str, bool]:
    # Build GraphQL query for all PRs at once
    query_parts = []
    for i, (issue_id, ref) in enumerate(pr_refs.items()):
        query_parts.append(f'''
            pr{i}: pullRequest(number: {ref.number}) {{
                isDraft
            }}
        ''')

    query = f'''
        query {{
            repository(owner: "{owner}", name: "{repo}") {{
                {" ".join(query_parts)}
            }}
        }}
    '''
    # Single gh api graphql call for all PRs
    stdout, stderr, rc = await runner([
        "gh", "api", "graphql", "-f", f"query={query}"
    ])
```

### Async File I/O

Session file reads use `aiofiles` with efficient tail-reading:

```python
async def check_worker_blocked(
    session_file: Path,
    n_lines: int = 10,
) -> tuple[bool, str | None]:
    async with aiofiles.open(session_file, mode="rb") as f:
        # Seek to end
        await f.seek(0, 2)
        file_size = await f.tell()

        # Read chunks from the end (only last N lines needed)
        chunk_size = 8192
        position = file_size
        buffer = b""
        # ... efficient reverse reading logic
```

### TTL Caching

Tmux session list cached with 2-second TTL using `async_lru`:

```python
from async_lru import alru_cache

@alru_cache(ttl=2.0)
async def get_tmux_sessions() -> list[str]:
    """Get all tmux session names (cached for 2 seconds)."""
    return await tmux.list_sessions()
```

### Dependency Injection via Protocol

External commands injectable for testing:

```python
class CommandRunner(Protocol):
    """Protocol for running external commands."""
    async def __call__(self, cmd: list[str]) -> tuple[str, str, int]:
        """Run command and return (stdout, stderr, returncode)."""
        ...

async def get_live_workers(
    short_id: str,
    *,
    runner: CommandRunner = default_runner,  # Default in production, mock in tests
) -> set[str]:
```

## Key Implementation Details

| File | Responsibility |
|------|----------------|
| `types.py` | All data structures: `ParsedIssue`, `FetchedIssueData`, `IssueState`, `CollectedState`. Status normalization via `IssueStatus.normalize()`. Session ID computation via `compute_session_id()`. |
| `fetch.py` | All I/O: tmux queries, GitHub GraphQL, session file reads. Exposes `fetch_all_issue_data()` as main entry point. |
| `decision.py` | Pure functions: `suggest_action()` returns action based on state, `build_collected_state()` assembles final output. Zero imports from I/O modules. |
| `cli.py` | CLI parsing, stdin handling, output formatting. Orchestrates fetch + decision. |
| `__init__.py` | Public API exports for external consumers. |

**Dependencies added**: `anyio`, `aiofiles`, `async_lru`

**Performance impact**: Parallel I/O reduces latency from O(n) sequential calls to O(1) parallel batch, with the slowest individual operation as the bottleneck rather than the sum.

## Prevention Strategies

### 1. Identify I/O Patterns Early

Before implementing any data collection or aggregation logic, audit all I/O operations:

| I/O Type | Question to Ask | Red Flag |
|----------|-----------------|----------|
| External commands | How many times will this run? | `for item in items: await run_cmd(...)` |
| API queries | Can these be batched? | N separate REST calls for N items |
| File reads | Are reads independent? | Sequential `await read_file()` calls |
| Database | N+1 query pattern? | Query per item in a loop |

### 2. Design for Parallelism First

Structure code to enable concurrent execution from the start:

```python
# BAD: Sequential by design
async def collect_all(items):
    results = []
    for item in items:
        data = await fetch_one(item)  # Sequential
        results.append(data)
    return results

# GOOD: Parallel by design
async def collect_all(items):
    async with anyio.create_task_group() as tg:
        results = {}
        async def fetch(item):
            results[item.id] = await fetch_one(item)
        for item in items:
            tg.start_soon(fetch, item)
    return results
```

### 3. Batch External Queries

When querying external services (APIs, databases), prefer batch operations:

```python
# BAD: N+1 queries
for issue_id in issue_ids:
    is_draft = await get_pr_draft_status(issue_id)  # 1 API call per issue

# GOOD: Single batch query
draft_map = await get_pr_draft_status_batch(pr_refs)  # 1 GraphQL query
```

### 4. Separate Fetch from Decision

Keep data fetching modules distinct from business logic:

```
src/module/
    fetch.py      # Pure I/O operations, async
    decision.py   # Pure business logic, sync
    types.py      # Data structures
```

Benefits:
- Fetch layer can be parallelized without touching decision logic
- Decision layer is trivially testable (no I/O mocking)
- Clear boundaries prevent I/O from creeping into business logic

### 5. Use Protocol-Based Dependency Injection

Define Protocols for external dependencies to enable testing and flexibility:

```python
from typing import Protocol

class CommandRunner(Protocol):
    async def __call__(self, cmd: list[str]) -> tuple[str, str, int]: ...

# Default implementation
async def default_runner(cmd: list[str]) -> tuple[str, str, int]:
    return await tmux.run(cmd)

# Functions accept optional runner
async def get_data(*, runner: CommandRunner = default_runner):
    stdout, stderr, rc = await runner(["some", "command"])
    ...
```

## Best Practices

### Async I/O Patterns

| Pattern | Library | Use Case |
|---------|---------|----------|
| Parallel task execution | `anyio.create_task_group()` | Running independent I/O concurrently |
| Async file reads | `aiofiles` | Non-blocking file I/O |
| Caching with TTL | `async_lru.alru_cache(ttl=N)` | Avoiding redundant I/O |
| Subprocess execution | `anyio.run_process()` or wrapper | External commands |

### Task Group Pattern

```python
async def fetch_all():
    # Results stored via closure
    workers: set[str] = set()
    draft_map: dict[str, bool] = {}

    async def fetch_workers():
        nonlocal workers
        workers = await get_live_workers()

    async def fetch_draft_status():
        nonlocal draft_map
        draft_map = await get_pr_draft_status_batch(pr_refs)

    async with anyio.create_task_group() as tg:
        tg.start_soon(fetch_workers)
        tg.start_soon(fetch_draft_status)

    # Both complete before we reach here
    return workers, draft_map
```

### Caching Strategy

Use TTL-based caching for data that is:
- Frequently accessed within a short window
- Expensive to fetch
- Tolerable to be slightly stale

```python
@alru_cache(ttl=2.0)  # 2-second TTL
async def get_tmux_sessions() -> list[str]:
    """Cached session list - avoids repeated subprocess calls."""
    return await tmux.list_sessions()
```

## Testing Approach

### How Dependency Injection Enables Testing

The `Protocol`-based dependency injection pattern allows tests to run without real I/O:

```python
# Production: uses real command runner
result = await get_pr_draft_status_batch(pr_refs)

# Test: inject mock runner
async def mock_runner(cmd: list[str]) -> tuple[str, str, int]:
    return json.dumps({"data": {...}}), "", 0

result = await get_pr_draft_status_batch(
    pr_refs,
    runner=mock_runner  # No subprocess spawned
)
```

### Testing Patterns

**1. Mock External Commands via Runner Injection**

```python
@pytest.mark.anyio
async def test_returns_draft_status_for_multiple_issues():
    async def mock_runner(cmd: list[str]) -> tuple[str, str, int]:
        response = {
            "data": {
                "repository": {
                    "pr0": {"isDraft": False},
                }
            }
        }
        return json.dumps(response), "", 0

    result = await get_pr_draft_status_batch(
        {"ENG-21": GitHubPRRef(owner="owner", repo="repo", number=1)},
        runner=mock_runner
    )
    assert result == {"ENG-21": False}
```

**2. Use `tmp_path` Fixture for File Tests**

```python
@pytest.mark.anyio
async def test_blocked_when_ask_user_question_pending(tmp_path: Path):
    session_file = tmp_path / "session.jsonl"
    session_file.write_text(
        '{"type":"assistant","message":{"content":[{"type":"tool_use","name":"AskUserQuestion","input":{"question":"Proceed?"}}]}}\n'
    )

    blocked, question = await check_worker_blocked(session_file)

    assert blocked is True
    assert question == "Proceed?"
```

**3. Test Decision Logic Independently (No Async)**

```python
def test_builds_state_with_action():
    # Pure data - no I/O needed
    data = FetchedIssueData(
        issue_id="ENG-21",
        status="Todo",
        labels=[],
        pr_is_draft=None,
        has_live_worker=False,
        is_blocked=False,
        blocked_question=None,
        has_user_feedback=False,
        has_user_input_needed=False,
    )

    state = build_issue_state(data, project_id)

    assert state.suggested_action == "dispatch_planner"
```

## Related Documentation

- [`docs/solutions/skill-patterns/parallel-subagent-background-execution.md`](../skill-patterns/parallel-subagent-background-execution.md) - Parallel execution at process level
- [`docs/solutions/integration-patterns/tmux-askuserquestion-navigation.md`](../integration-patterns/tmux-askuserquestion-navigation.md) - tmux automation patterns
- Reference implementation: `src/legion/state/fetch.py`
