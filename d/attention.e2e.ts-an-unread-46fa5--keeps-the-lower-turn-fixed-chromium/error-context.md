# Instructions

- Following Playwright test failed.
- Explain why, be concise, respect Playwright best practices.
- Provide a snippet of code with the fix, if possible.

# Test info

- Name: attention.e2e.ts >> an unread divider landing in a Conversation gap keeps the lower turn fixed
- Location: e2e/attention.e2e.ts:226:1

# Error details

```
Error: Command failed: psql postgres://postgres:ci@localhost:5432/postgres?sslmode=disable -v ON_ERROR_STOP=1 -c TRUNCATE TABLE agent_tokens, repo_projects, user_issue_state, events, refs, messages, comments, asks, doc_checkpoints, doc_snapshots, doc_updates, artifact_versions, artifacts, issue_external_links, issues, projects, users RESTART IDENTITY CASCADE
ERROR:  deadlock detected
DETAIL:  Process 245 waits for AccessExclusiveLock on relation 16450 of database 5; blocked by process 184.
Process 184 waits for AccessShareLock on relation 16470 of database 5; blocked by process 245.
HINT:  See server log for query details.

```