// The delivery pages' e2e fixture, written directly in Postgres: the timeline and the measures
// read only stored facts, so these are the rows intake and reconcile would have written, without
// a live GitHub App. One module so the timeline rows, the measures rows and the docs screenshots
// read the same data. acme/widgets-shaped placeholders throughout (AGENTS.md: "This repository is
// public").
import { sql } from "./psql";

const SEED_ACTOR = `'{"kind":"system","id":"e2e-seed"}'`;

const SETTINGS = `INSERT INTO delivery_settings (
     singleton, deploy_repo, deploy_workflow_path, production_job_name,
     pr_checks_workflow_path, population_authors, excluded_repos, updated_by
   ) VALUES (
     true, 'acme/widgets', '.github/workflows/deploy.yml', 'widgets-release / widgets-release',
     '.github/workflows/pr-checks.yml', ARRAY['octocat'], ARRAY[]::text[], ${SEED_ACTOR}
   )`;

/** The slice 1 timeline's fixture, in 2024-06-01: two population pull requests (#1 shipped by
 *  deploy run 500 and attributed to ACME-1, a P0 issue; #2 waiting, a rework, with no issue),
 *  the deploy run with its jobs, and a failed run (501). */
export async function seedDeliveryFixture(): Promise<void> {
  await sql(
    SETTINGS,
    `INSERT INTO projects (key, name) VALUES ('ACME', 'Acme') ON CONFLICT DO NOTHING`,
    `INSERT INTO issues (key, project_key, number, title, status, priority, created_by, rank)
     VALUES ('ACME-1', 'ACME', 1, 'Ship the widgets', 'todo', 0, ${SEED_ACTOR}, 'U')`,
    `INSERT INTO delivery_pull_requests (
       repo, number, title, url, author, created_at, merged_at, additions, deletions, rework, issue_key, sessions, partial
     ) VALUES
       ('acme/widgets', 1, 'feat: a shipped widget', 'https://github.com/acme/widgets/pull/1', 'octocat',
        '2024-06-01T00:00:00Z', '2024-06-01T01:00:00Z', 12, 3, false, 'ACME-1', ARRAY[]::text[], false),
       ('acme/widgets', 2, 'fix: a waiting widget', 'https://github.com/acme/widgets/pull/2', 'octocat',
        '2024-06-01T02:00:00Z', '2024-06-01T03:00:00Z', 4, 1, true, null, ARRAY[]::text[], false)`,
    `INSERT INTO delivery_runs (
       repo, run_id, kind, head_sha, head_commit_at, started_at, completed_at, conclusion, url
     ) VALUES
       ('acme/widgets', 500, 'deploy', 'deadbeef', '2024-06-01T01:00:00Z',
        '2024-06-01T01:30:00Z', '2024-06-01T01:40:00Z', 'success', 'https://github.com/acme/widgets/actions/runs/500'),
       ('acme/widgets', 501, 'deploy', 'cafef00d', '2024-06-01T03:00:00Z',
        '2024-06-01T12:00:00Z', '2024-06-01T12:20:00Z', 'failure', 'https://github.com/acme/widgets/actions/runs/501')`,
    `INSERT INTO delivery_run_jobs (repo, run_id, name, started_at, completed_at, conclusion) VALUES
       ('acme/widgets', 500, 'build', '2024-06-01T01:30:00Z', '2024-06-01T01:32:00Z', 'success'),
       ('acme/widgets', 500, 'widgets-release / widgets-release', '2024-06-01T01:32:00Z', '2024-06-01T01:40:00Z', 'success'),
       ('acme/widgets', 501, 'build', '2024-06-01T12:00:00Z', '2024-06-01T12:05:00Z', 'success'),
       ('acme/widgets', 501, 'integration tests', '2024-06-01T12:05:00Z', '2024-06-01T12:20:00Z', 'failure'),
       ('acme/widgets', 501, 'widgets-release / widgets-release', null, null, 'skipped')`
  );
}

/** The measures fixture's window (packages/envoy/internal/dispatch/delivery/measures/testdata/
 *  dora-fixture.json, the prototype's own web/fixtures/dataset.json with its names replaced). */
export const FIXTURE_WINDOW = { from: "2026-08-30T00:00:00Z", to: "2026-09-27T20:00:00Z" } as const;

/** The measures fixture: the prototype fixture's 14 population pull requests and 8 deploy runs,
 *  each run's production job (`widgets-release / widgets-release`) and failed jobs, and project
 *  ACME's four issues, ACME-103 the one open P0 with neither a claim nor a route. Every run is a
 *  push on main. Its expected measures are the prototype's own computeDora with each deploy derived
 *  by its own containment (LEGION-567 slice 2 plan, "How the expected values are produced"). */
export async function seedMeasuresFixture(): Promise<void> {
  await sql(
    SETTINGS,
    `INSERT INTO projects (key, name) VALUES ('ACME', 'Acme') ON CONFLICT DO NOTHING`,
    `INSERT INTO issues (key, project_key, number, title, status, priority, route, claimed_by, claimed_at, created_by, rank) VALUES
       ('ACME-100', 'ACME', 100, 'Build timeline UI', 'in_progress', 1, null,
        '{"kind":"session","id":"01a1-e2e-session"}', now(), ${SEED_ACTOR}, 'U100'),
       ('ACME-101', 'ACME', 101, 'DORA metrics module', 'in_progress', 2, 'role:e2e', null, null, ${SEED_ACTOR}, 'U101'),
       ('ACME-102', 'ACME', 102, 'Collector schema', 'done', 1, null, null, null, ${SEED_ACTOR}, 'U102'),
       ('ACME-103', 'ACME', 103, 'Production deploy gate flakes', 'triage', 0, null, null, null, ${SEED_ACTOR}, 'U103')`,
    `INSERT INTO delivery_pull_requests (
       repo, number, title, url, author, created_at, merged_at, first_commit_at,
       additions, deletions, rework, issue_key, sessions, partial
     ) VALUES
       ('acme/widgets', 101, 'Add timeline zoom brush', 'https://github.com/acme/widgets/pull/101', 'octocat',
        '2026-09-04T18:00:00Z', '2026-09-05T12:00:00Z', '2026-09-04T15:00:00Z', 210, 12, false, 'ACME-100', ARRAY[]::text[], false),
       ('acme/widgets', 102, 'fix: timeline brush jumps on resize', 'https://github.com/acme/widgets/pull/102', 'octocat',
        '2026-09-06T09:00:00Z', '2026-09-06T13:00:00Z', '2026-09-06T08:30:00Z', 18, 4, true, 'ACME-100', ARRAY[]::text[], false),
       ('acme/widgets', 103, 'Add DORA lead-time calc', 'https://github.com/acme/widgets/pull/103', 'octocat',
        '2026-09-07T10:00:00Z', '2026-09-07T16:00:00Z', '2026-09-07T08:00:00Z', 140, 6, false, 'ACME-101', ARRAY[]::text[], false),
       ('acme/widgets', 104, 'Add facet filtering module', 'https://github.com/acme/widgets/pull/104', 'octocat',
        '2026-09-08T09:00:00Z', '2026-09-08T15:00:00Z', '2026-09-08T07:00:00Z', 95, 3, false, 'ACME-100', ARRAY[]::text[], false),
       ('acme/widgets', 105, 'revert: Add facet filtering module', 'https://github.com/acme/widgets/pull/105', 'octocat',
        '2026-09-09T08:00:00Z', '2026-09-09T09:00:00Z', '2026-09-09T07:45:00Z', 3, 95, true, 'ACME-100', ARRAY[]::text[], false),
       ('acme/widgets', 106, 'Add drill-down panel', 'https://github.com/acme/widgets/pull/106', 'octocat',
        '2026-09-10T09:00:00Z', '2026-09-10T17:00:00Z', '2026-09-10T08:00:00Z', 180, 20, false, 'ACME-100', ARRAY[]::text[], false),
       ('acme/widgets', 107, 'hotfix: drill-down panel crash on null issue', 'https://github.com/acme/widgets/pull/107', 'octocat',
        '2026-09-11T10:00:00Z', '2026-09-11T11:00:00Z', '2026-09-11T09:45:00Z', 9, 2, true, 'ACME-100', ARRAY[]::text[], false),
       ('acme/gadgets', 20, 'Add dispatch API client', 'https://github.com/acme/gadgets/pull/20', 'octocat',
        '2026-09-12T09:00:00Z', '2026-09-12T14:00:00Z', '2026-09-12T08:00:00Z', 60, 0, false, 'ACME-102', ARRAY[]::text[], false),
       ('acme/widgets', 108, 'Wire SSE events endpoint', 'https://github.com/acme/widgets/pull/108', 'octocat',
        '2026-09-13T07:00:00Z', '2026-09-13T08:00:00Z', '2026-09-13T06:00:00Z', 75, 5, false, 'ACME-100', ARRAY[]::text[], false),
       ('acme/widgets', 109, 'Add priority facet counts', 'https://github.com/acme/widgets/pull/109', 'octocat',
        '2026-09-13T08:00:00Z', '2026-09-13T08:30:00Z', '2026-09-13T07:00:00Z', 40, 2, false, 'ACME-101', ARRAY[]::text[], false),
       ('acme/widgets', 110, 'Improve waiting-to-deploy step line', 'https://github.com/acme/widgets/pull/110', 'octocat',
        '2026-09-15T09:00:00Z', '2026-09-15T15:00:00Z', '2026-09-15T08:00:00Z', 30, 8, false, 'ACME-100', ARRAY[]::text[], false),
       ('acme/widgets', 111, 'Add component hierarchy filter', 'https://github.com/acme/widgets/pull/111', 'octocat',
        '2026-09-16T09:00:00Z', '2026-09-16T16:00:00Z', '2026-09-16T08:00:00Z', 120, 10, false, null, ARRAY[]::text[], false),
       ('acme/widgets', 112, 'fix: component hierarchy includes grandchildren', 'https://github.com/acme/widgets/pull/112', 'octocat',
        '2026-09-17T07:00:00Z', '2026-09-17T07:30:00Z', '2026-09-17T06:45:00Z', 14, 3, true, null, ARRAY[]::text[], false),
       ('acme/widgets', 113, 'Add rework share metric', 'https://github.com/acme/widgets/pull/113', 'octocat',
        '2026-09-18T09:00:00Z', '2026-09-18T15:00:00Z', '2026-09-18T08:00:00Z', 50, 4, false, 'ACME-101', ARRAY[]::text[], false)`,
    `INSERT INTO delivery_runs (
       repo, run_id, kind, head_sha, head_commit_at, started_at, completed_at, conclusion, url
     ) VALUES
       ('acme/widgets', 501, 'deploy', 'aaa101', '2026-09-05T12:30:00Z', '2026-09-05T12:00:00Z',
        '2026-09-05T13:05:00Z', 'success', 'https://github.com/acme/widgets/actions/runs/501'),
       ('acme/widgets', 502, 'deploy', 'aaa102', '2026-09-06T13:30:00Z', '2026-09-06T13:35:00Z',
        '2026-09-06T14:05:00Z', 'success', 'https://github.com/acme/widgets/actions/runs/502'),
       ('acme/widgets', 503, 'deploy', 'aaa103', '2026-09-09T09:30:00Z', '2026-09-09T09:35:00Z',
        '2026-09-09T10:05:00Z', 'success', 'https://github.com/acme/widgets/actions/runs/503'),
       ('acme/widgets', 504, 'deploy', 'aaa104', '2026-09-13T08:45:00Z', '2026-09-13T08:50:00Z',
        '2026-09-13T09:05:00Z', 'success', 'https://github.com/acme/widgets/actions/runs/504'),
       ('acme/widgets', 505, 'deploy', 'aaa105', '2026-09-17T07:45:00Z', '2026-09-17T07:50:00Z',
        '2026-09-17T08:05:00Z', 'success', 'https://github.com/acme/widgets/actions/runs/505'),
       ('acme/widgets', 506, 'deploy', 'aaa106', '2026-09-11T08:00:00Z', '2026-09-11T08:30:00Z',
        '2026-09-11T09:05:00Z', 'failure', 'https://github.com/acme/widgets/actions/runs/506'),
       ('acme/widgets', 507, 'deploy', 'aaa107', '2026-09-14T08:30:00Z', '2026-09-14T08:35:00Z',
        '2026-09-14T09:05:00Z', 'failure', 'https://github.com/acme/widgets/actions/runs/507'),
       ('acme/widgets', 508, 'deploy', 'aaa108', '2026-09-19T09:50:00Z', '2026-09-19T10:00:00Z',
        null, 'cancelled', 'https://github.com/acme/widgets/actions/runs/508')`,
    `INSERT INTO delivery_run_jobs (repo, run_id, name, completed_at, conclusion) VALUES
       ('acme/widgets', 501, 'widgets-release / widgets-release', '2026-09-05T13:00:00Z', 'success'),
       ('acme/widgets', 502, 'widgets-release / widgets-release', '2026-09-06T14:00:00Z', 'success'),
       ('acme/widgets', 503, 'widgets-release / widgets-release', '2026-09-09T10:00:00Z', 'success'),
       ('acme/widgets', 504, 'widgets-release / widgets-release', '2026-09-13T09:00:00Z', 'success'),
       ('acme/widgets', 505, 'widgets-release / widgets-release', '2026-09-17T08:00:00Z', 'success'),
       ('acme/widgets', 506, 'integration-test', '2026-09-11T09:00:00Z', 'failure'),
       ('acme/widgets', 507, 'widgets-release / widgets-release', '2026-09-14T09:00:00Z', 'failure')`
  );
}
