import type { Actor } from "./dispatch-api-core";

/**
 * The delivery timeline: the merges, deploys and pipeline runs `GET /api/v1/delivery/*` answers
 * with, the measures computed from them, and the one settings record that says which repository
 * and which workflows they are read from.
 */

/** One job of a `DeliveryRun`: the two fields the delivery timeline's drill-down shows for a
 *  failed job, in `failed_jobs` and `root_failing_job`. */
export interface DeliveryRunJob {
  readonly name: string;
  readonly completed_at: string | null;
}

/** A concluded job's result, as `delivery_run_jobs` stores it. */
export type DeliveryJobConclusion = "success" | "failure" | "cancelled" | "skipped" | "timed_out";

/**
 * One population pull request on `GET /api/v1/delivery/timeline` (LEGION-567): the facts LEGION-294
 * defines plus what the server derives at read time from the stored facts: `deployed_status`, the
 * linked issue's title, priority and effective components (the nearest issue on its parent chain
 * that chose, as every issue read resolves them), and (on `DeliveryRun`) `root_failing_job`.
 */
export interface DeliveryPR {
  /** "owner/repo#N". */
  readonly id: string;
  readonly repo: string;
  readonly number: number;
  readonly title: string;
  readonly url: string;
  readonly author: string;
  readonly created_at: string | null;
  readonly merged_at: string | null;
  readonly first_commit_at: string | null;
  readonly additions: number | null;
  readonly deletions: number | null;
  /** True until the completing GitHub fetch (or a reconcile pass) fills the row. */
  readonly partial: boolean;
  readonly rework: boolean;
  readonly issue: string | null;
  /** The linked issue's title; null with no issue, or one Dispatch does not hold. */
  readonly issue_title: string | null;
  /** The linked issue's priority, `P0` (highest) to `P3`; null with no issue or none set. */
  readonly priority: "P0" | "P1" | "P2" | "P3" | null;
  /** The linked issue's effective components, each `<project>/<id>` (a key of the response's
   *  `components`); empty with no issue or none attached. */
  readonly components: readonly string[];
  readonly sessions: readonly string[];
  /**
   * The Parent agent: the session group holding the first of `sessions` (as `group:<id>`; the
   * response's `session_groups` names it), else that session's latest Dispatch title, or its bare
   * id if none was ever recorded. A session outside every group whose title is a group's name
   * carries its id beside the title (`Planner (<id>)`). Null when no session wrote the pull
   * request. `session` (the `sessions` field) keeps each raw id.
   */
  readonly parent_agent: string | null;
  readonly deploy_run: number | null;
  readonly deployed_at: string | null;
  readonly deployed_status: "deployed" | "waiting" | "not_tracked";
  /** Set once this pull request's completing fetch answered a permanent 404/410 from GitHub (or
   *  could not resolve which installation covers its repository): why, for the drill-down. Null
   *  for every normal row. */
  readonly unfetchable_reason: string | null;
}

/** A run's production job (the one of `production_job_names` it holds): its conclusion as a deploy
 *  reads it, `skipped` for a job that succeeded with its apply step not succeeding, and when it
 *  finished. A successful one is a production deploy, drawn at `completed_at`; a failed one is a
 *  pipeline failure even when no other job failed. */
export interface DeliveryRunProduction {
  readonly conclusion: DeliveryJobConclusion | null;
  readonly completed_at: string | null;
}

/** One run of the deploy workflow on `GET /api/v1/delivery/timeline` (the server returns deploy
 *  runs only): a production deploy (`production` succeeded; sized by `prs`) and/or a pipeline
 *  failure (`failed_jobs`, `root_failing_job`, or a failed `production`). */
export interface DeliveryRun {
  readonly id: number;
  readonly url: string;
  readonly head_sha: string;
  readonly head_at: string;
  readonly started_at: string;
  readonly completed_at: string | null;
  readonly conclusion: "success" | "failure" | "cancelled" | null;
  /** Null when the run has no production job. */
  readonly production: DeliveryRunProduction | null;
  /** Every failed job, by when each started (a job that never started last), then by name in
   *  byte order. */
  readonly failed_jobs: readonly DeliveryRunJob[];
  /** The earliest-finishing failed job that isn't a summary/guard job, falling back to the
   *  earliest failed job overall, the first by name in byte order among those finishing together
   *  (a job that never finished last); null on a run with no failed job. */
  readonly root_failing_job: DeliveryRunJob | null;
  /** Every window pull request this run shipped first, whatever facets the request names, by
   *  `merged_at`, then by `id` in byte order. Of two production deploys completing in the same
   *  second, the one with the lower run id ships a pull request both could. */
  readonly prs: readonly { readonly id: string; readonly title: string }[];
}

/** The timeline's facets, as the query parameters that select them and the `facet_counts` keys
 *  that count them. */
export type DeliveryFacet =
  | "repo"
  | "parent_agent"
  | "session"
  | "issue"
  | "priority"
  | "component"
  | "author"
  | "rework"
  | "deployed";

/** The facets the page colours merges by, as the `color_counts` keys that count them. */
export type DeliveryColorFacet = "repo" | "author" | "priority" | "component" | "parent_agent";

/** One architecture component a pull request's issue can carry; `parent` is another key of the
 *  response's `components`, and a component facet selection of a parent includes its children. */
export interface DeliveryComponent {
  readonly title: string;
  readonly parent: string | null;
}

/** One pull request of the deploy repository that merged before the timeline's window and had
 *  not shipped by its start, matching the request's facets and search: the waiting-to-deploy line
 *  starts the window counting it, and stops when it ships (`deployed_at`, null while it waits). */
export interface DeliveryWaitingPR {
  readonly merged_at: string;
  readonly deployed_at: string | null;
}

/**
 * `GET /api/v1/delivery/timeline?from&to&q&<facets>`: merges, deploys, pipeline failures and
 * waiting-to-deploy PRs within `[from, to)`, the search and the given facets. `runs` holds the
 * deploy workflow's runs only. `waiting` holds the pull requests that merged before `from` and had
 * not shipped by then, under the same search and facets. `facet_counts` counts, per facet, the
 * window's pull requests by that facet's values with the search and every other facet applied and
 * its own selection ignored, so picking a value never zeroes its own count; `color_counts` counts
 * them by each colour-by facet's value with nothing applied, so a value keeps its colour while
 * facets change. A pull request with no issue counts under `__no_issue__` for priority and
 * component, an issue without either under `__no_priority__` / `__no_component__`, and a pull
 * request naming no session under `__no_session__` for parent agent; each placeholder also
 * selects. `components` names every component of the projects the window's issues belong to,
 * `issue_titles` every issue its pull requests name. `measures` is what
 * `GET /api/v1/delivery/measures` answers for the same window, search and facets, computed from the
 * pull requests this read already holds, so a page showing both reads them once.
 */
export interface DeliveryTimelineResponse {
  readonly window: { readonly from: string; readonly to: string };
  readonly prs: readonly DeliveryPR[];
  readonly waiting: readonly DeliveryWaitingPR[];
  readonly runs: readonly DeliveryRun[];
  readonly facet_counts: Readonly<Record<DeliveryFacet, Readonly<Record<string, number>>>>;
  readonly color_counts: Readonly<Record<DeliveryColorFacet, Readonly<Record<string, number>>>>;
  readonly components: Readonly<Record<string, DeliveryComponent>>;
  readonly issue_titles: Readonly<Record<string, string>>;
  readonly freshness: {
    readonly last_event_at: string | null;
    readonly last_reconcile_at: string | null;
    /** The most recent reconcile pass's failure (a missing permission, a rate limit, any other
     *  error), named rather than a generic staleness indicator; null once a pass has succeeded. */
    readonly last_error: string | null;
    /** Population pull requests whose completing fetch answered a permanent 404/410 from GitHub
     *  (the pull request or its repository no longer exists, or no longer reaches the App) --
     *  never retried again automatically. */
    readonly unfetchable_count: number;
  };
  readonly measures: DeliveryMeasuresResponse;
  /** Every session group, by name. */
  readonly session_groups: readonly DeliverySessionGroup[];
  /** For each session that is the first session of a window pull request, its title, or its id
   *  when it has none. A group member no window pull request names first is absent. */
  readonly session_titles: Readonly<Record<string, string>>;
  /** For each such session, how many window pull requests it is the first session of, before the
   *  search and the facets. */
  readonly session_pr_counts: Readonly<Record<string, number>>;
}

/** One job of `GET /api/v1/delivery/runs/{id}`, with its result and timings. */
export interface DeliveryRunJobDetail {
  readonly name: string;
  readonly started_at: string | null;
  readonly completed_at: string | null;
  readonly conclusion: DeliveryJobConclusion | null;
}

/** `GET /api/v1/delivery/runs/{id}`: one deploy-repository run with every job it ran, by start (a
 *  job that never started last, then by name). */
export interface DeliveryRunDetail {
  readonly id: number;
  readonly url: string;
  readonly jobs: readonly DeliveryRunJobDetail[];
}

/** A set of durations' median, 90th percentile (linear between the two nearest ranks) and maximum,
 *  in minutes; all three null for an empty set. */
export interface DeliverySpread {
  readonly median_minutes: number | null;
  readonly p90_minutes: number | null;
  readonly max_minutes: number | null;
}

/** One UTC day of the measures' window; `partial` when the window covers only part of it. Runs
 *  count on the day they started, so the days sum to the window's totals. */
export interface DeliveryDailyPoint {
  /** `YYYY-MM-DD`, UTC. */
  readonly day: string;
  readonly partial: boolean;
  /** Runs whose production job succeeded. */
  readonly deploys: number;
  /** Runs that concluded success or failure (a cancelled run is not a deploy attempt). */
  readonly concluded: number;
  /** Concluded runs whose production job succeeded. */
  readonly reached_production: number;
  readonly cancelled: number;
  /** `reached_production / concluded`; null when nothing concluded. */
  readonly run_success_rate: number | null;
}

/** Failed-change flags by state against the population: a flag counts only when its broken pull
 *  request is in it, and only a confirmed one counts toward `rate`. */
export interface DeliveryFlagCounts {
  readonly confirmed: number;
  readonly pending: number;
  readonly rejected: number;
  /** Confirmed flags whose fix is a revert. */
  readonly reverts: number;
  /** Pull requests (per PR) or successful deploys (per deploy). */
  readonly total: number;
  /** `confirmed / total`; 0 when `total` is 0. */
  readonly rate: number;
}

/** The per-deploy change failure rate: each count is deploys that shipped a flagged pull request,
 *  each deploy once. */
export interface DeliveryDeployFlagCounts extends DeliveryFlagCounts {
  /** Deploys with a confirmed or a pending flag. */
  readonly confirmed_or_pending: number;
  /** The rate if every pending flag were confirmed: `confirmed_or_pending / total`. */
  readonly upper_bound_rate: number;
}

/** The delivery measures (LEGION-294's definitions) over one window's selected population. */
export interface DeliveryMeasures {
  /** Successful production deploys on main started in the window; `deploys_with_prs` those that
   *  shipped at least one pull request merged in the window, whatever the facets. */
  readonly deploy_frequency: {
    readonly successful_deploys: number;
    readonly deploys_with_prs: number;
    readonly per_day: number;
    readonly with_prs_per_day: number;
  };
  /** Merge, first commit and open to production over the deployed pull requests; open to merge
   *  over every one. */
  readonly lead_time: {
    readonly merge_to_production: DeliverySpread;
    readonly first_commit_to_production: DeliverySpread;
    readonly opened_to_production: DeliverySpread;
    readonly opened_to_merge: DeliverySpread;
  };
  readonly change_failure_rate: {
    readonly per_pr: DeliveryFlagCounts;
    readonly per_deploy: DeliveryDeployFlagCounts;
  };
  /** Median minutes from a broken pull request's deploy to its fix's, over confirmed flags whose
   *  two pull requests both deployed; null when there is none. */
  readonly time_to_restore: { readonly median_minutes: number | null };
  /** Rework pull requests over all; 0 with none. */
  readonly rework_share: number;
  /** Deploy runs that concluded success or failure, those of them that reached production, the
   *  cancelled runs beside them, and the rate (null when none concluded). */
  readonly deploy_run_success: {
    readonly concluded: number;
    readonly reached_production: number;
    readonly cancelled: number;
    readonly rate: number | null;
  };
  /** Every UTC day of the window, oldest first. */
  readonly daily: readonly DeliveryDailyPoint[];
}

/** Sami's engineering targets: "under" targets are met strictly below, "at least" targets at or
 *  above. */
export interface DeliveryTargets {
  /** At least. */
  readonly deploys_per_day: number;
  /** Under, per deploy. */
  readonly change_failure_rate: number;
  /** Under, for every change: judged on the maximum. */
  readonly merge_to_production_minutes: number;
  /** Under, judged on the median. */
  readonly opened_to_merge_median_minutes: number;
  /** At least. */
  readonly deploy_run_success_rate: number;
  /** Exactly. */
  readonly unowned_p0: number;
}

/** Whether each measure meets its target; null when the window has nothing to measure. */
export interface DeliveryKpiStatus {
  readonly deploys_per_day: boolean;
  readonly change_failure_rate: boolean | null;
  readonly change_failure_rate_upper_bound: boolean | null;
  readonly merge_to_production_median: boolean | null;
  /** Every change: the maximum. */
  readonly merge_to_production: boolean | null;
  readonly opened_to_merge: boolean | null;
  readonly deploy_run_success: boolean | null;
  readonly unowned_p0: boolean;
}

/** One open P0 issue with neither a claim nor a route. */
export interface DeliveryUnownedIssue {
  readonly key: string;
  readonly title: string;
}

/**
 * `GET /api/v1/delivery/measures?from&to&q&<facets>`: the delivery measures for `[from, to)` over
 * the pull requests the timeline's search and facets select, computed on request from stored
 * facts. Facets narrow the pull-request measures; deploys and runs follow the window alone.
 * `flags_source` says whether stored failed-change flags fed the change failure rate and time to
 * restore (`none`: no flags are stored, so both count nothing). `unowned_p0` spans every project
 * and follows neither the window nor the facets; `status` judges `measures` and it against
 * `targets`.
 */
export interface DeliveryMeasuresResponse {
  readonly window: DeliveryTimelineResponse["window"];
  readonly computed_at: string;
  readonly measures: DeliveryMeasures;
  readonly flags_source: "none" | "stored";
  readonly unowned_p0: readonly DeliveryUnownedIssue[];
  readonly targets: DeliveryTargets;
  readonly status: DeliveryKpiStatus;
  readonly freshness: DeliveryTimelineResponse["freshness"];
}

/** One shown UTC day's, or the shown days', results of one Pipeline stage. Only results that ended
 *  in success or failure are timed; a cancelled one is counted beside them, and anything else
 *  (skipped, still running) is not counted at all. */
export interface DeliveryTimingCell {
  /** Timed results: success or failure. */
  readonly n: number;
  /** The timed results' median minutes, to one decimal; null when none. */
  readonly median_minutes: number | null;
  readonly p90_minutes: number | null;
  readonly success: number;
  /** Failures, a timed-out result among them. */
  readonly failure: number;
  readonly cancelled: number;
  /** `success / (success + failure)`, to four decimals; null when both are 0. */
  readonly success_rate: number | null;
}

/** One shown UTC day of a Pipeline stage; `partial` is today. */
export interface DeliveryTimingDay extends DeliveryTimingCell {
  /** `YYYY-MM-DD`, UTC: the day each run started (a job by its run's start). */
  readonly day: string;
  readonly partial: boolean;
}

/** One Pipeline stage per shown day, oldest first, and over all of them. */
export interface DeliveryTiming {
  readonly days: readonly DeliveryTimingDay[];
  readonly total: DeliveryTimingCell;
}

/** One PR-checks job's timing, by its name as a run lists it. */
export interface DeliveryJobTiming extends DeliveryTiming {
  readonly name: string;
}

/** One deploy-workflow job's timing in push runs on main, by the job names the card follows, joined
 *  by `or`. */
export interface DeliveryDeployJobTiming extends DeliveryTiming {
  readonly job: string;
}

/** A PR-checks job that failed in the last 24 hours: its failures there, out of how many timed
 *  results, and the run of its latest failure. */
export interface DeliveryFailingJob {
  readonly name: string;
  readonly failures: number;
  readonly results: number;
  readonly last_failed_at: string;
  readonly last_run_url: string;
}

/** One shown merge day's, or the shown days', lead times in minutes, unrounded. */
export interface DeliveryLeadTimeCell {
  readonly n: number;
  readonly median_minutes: number | null;
  readonly p90_minutes: number | null;
  readonly max_minutes: number | null;
}

/** One shown UTC merge day's lead times; `partial` is today. */
export interface DeliveryLeadTimeDay extends DeliveryLeadTimeCell {
  readonly day: string;
  readonly partial: boolean;
}

/** One lead time per shown merge day, oldest first, and over all of them. */
export interface DeliveryLeadTimeTiming {
  readonly days: readonly DeliveryLeadTimeDay[];
  readonly total: DeliveryLeadTimeCell;
}

/**
 * `GET /api/v1/delivery/pipeline?at`: the Pipeline page over the seven UTC days ending on `at`'s
 * (default now), computed on request from stored facts and followed by neither the timeline's
 * window nor its facets. `pr_checks` is the PR-checks workflow's pull_request runs: their whole
 * run time, their five slowest jobs (a job needs 50 timed results in the seven days to rank), and
 * the jobs that failed in the 24 hours before `at`, the roll-up job the settings name left out.
 * The three deploy jobs are timed in the deploy workflow's push runs on main;
 * `staging_e2e` and `staging_apply` are null when the settings name no such job. The two lead
 * times bucket every population pull request merged in the shown days by its merge day, judged
 * against `targets`.
 */
export interface DeliveryPipelineResponse {
  /** The seven shown UTC days, oldest first; the last is `at`'s. */
  readonly days: readonly string[];
  readonly computed_at: string;
  readonly pr_checks: {
    readonly run: DeliveryTiming;
    readonly slowest_jobs: readonly DeliveryJobTiming[];
    readonly failing_jobs: readonly DeliveryFailingJob[];
  };
  readonly staging_e2e: DeliveryDeployJobTiming | null;
  readonly staging_apply: DeliveryDeployJobTiming | null;
  readonly production_apply: DeliveryDeployJobTiming;
  readonly merge_to_production: DeliveryLeadTimeTiming;
  readonly opened_to_merge: DeliveryLeadTimeTiming;
  readonly targets: DeliveryTargets;
  readonly freshness: DeliveryTimelineResponse["freshness"];
}

/**
 * The delivery timeline's one configuration record: `GET /api/v1/settings/delivery` answers it, or
 * `null` until someone sets it, and `PUT /api/v1/settings/delivery` answers the record it stored.
 * Which repository deploys, its deploy and PR-checks workflow paths, the deploy workflow's
 * production jobs and optional apply step, whose merged pull requests count, and the three optional
 * job names the Pipeline page follows besides the production jobs. The last three timestamps and
 * the error are the reconcile's own, cleared by every write.
 */
export interface DeliverySettings {
  /** `owner/repo`, lowercased as the server stores it. */
  readonly deploy_repo: string;
  readonly deploy_workflow_path: string;
  /** The deploy workflow's production jobs, as a run lists each (a reusable workflow's job reads
   *  `<caller> / <job>`): a run's production job is the one whose name is here. A deploy repository
   *  that renames its job lists the old name beside the new, so history and new runs both count. */
  readonly production_job_names: readonly string[];
  /** The production job's step whose success means it applied, as GitHub lists the step; null when
   *  any successful production job is a deploy. A production job that succeeded with this step not
   *  succeeding (a superseded run that stood down) is no deploy. */
  readonly production_apply_step_name: string | null;
  readonly pr_checks_workflow_path: string;
  /** GitHub logins as GitHub's REST API spells them, compared exactly: an App's ends in `[bot]`. */
  readonly population_authors: readonly string[];
  readonly excluded_repos: readonly string[];
  /** The deploy workflow's staging end-to-end job, as a run lists it; null when the pipeline has
   *  none, and the Pipeline page omits its card. */
  readonly staging_e2e_job_name: string | null;
  /** The deploy workflow's staging deploy job; null when the pipeline has none. */
  readonly staging_apply_job_name: string | null;
  /** The PR-checks workflow's roll-up job, which only reports the other jobs' result and which the
   *  failing-jobs table leaves out; null leaves nothing out. */
  readonly pr_checks_rollup_job_name: string | null;
  readonly last_event_at: string | null;
  readonly last_reconcile_at: string | null;
  readonly last_error: string | null;
  readonly updated_by: Actor;
  readonly updated_at: string;
}

/** `PUT /api/v1/settings/delivery`'s body, which replaces the whole record. Production job names
 *  are trimmed and must contain at least one unique non-blank name; the apply step and the three
 *  Pipeline job names are optional, and absent, null or blank values store null. */
export interface DeliverySettingsInput {
  readonly deploy_repo: string;
  readonly deploy_workflow_path: string;
  readonly production_job_names: readonly string[];
  readonly production_apply_step_name?: string | null;
  readonly pr_checks_workflow_path: string;
  readonly population_authors: readonly string[];
  readonly excluded_repos: readonly string[];
  readonly staging_e2e_job_name?: string | null;
  readonly staging_apply_job_name?: string | null;
  readonly pr_checks_rollup_job_name?: string | null;
}

/**
 * One of the Delivery page's session groups: a stable id, a name and the agent sessions it holds.
 * A pull request whose first session is a member has the group as its Parent agent, under the
 * value `group:<id>`, so a rename keeps every filter and link that names it. A session is in at
 * most one group, and a group's name may equal a session title only when the group holds every
 * session with that title. The timeline lists every group (`DeliveryTimelineResponse.session_groups`).
 */
export interface DeliverySessionGroup {
  readonly id: string;
  readonly name: string;
  /** Session ids, sorted. */
  readonly sessions: readonly string[];
  readonly created_by: Actor;
  readonly created_at: string;
  readonly updated_by: Actor;
  readonly updated_at: string;
}

/** `POST /api/v1/delivery/session-groups`'s body. */
export interface CreateDeliverySessionGroupInput {
  readonly name: string;
  readonly sessions: readonly string[];
}

/** `PATCH /api/v1/delivery/session-groups/{id}`'s body: each field optional. */
export interface UpdateDeliverySessionGroupInput {
  readonly name?: string;
  readonly add?: readonly string[];
  readonly remove?: readonly string[];
}
