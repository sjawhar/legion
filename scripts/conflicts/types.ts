export type MergeKind = "forward" | "other-branch" | "internal";
export type FixState = "before" | "transition" | "after";

/** One merge commit on a pull request, as the Python script recorded it (key names kept). */
export interface MergeRecord {
  oid: string;
  date: string; // committer date, RFC 3339
  headline: string;
  parents: string[];
  kind: MergeKind;
  branch_parent?: string; // absent for kind "internal"
  upstream_parent?: string;
  upstream_on_main?: boolean;
  upstream_on_base?: boolean;
  conflicts?: string[] | null; // sorted unique paths; [] clean; null undeterminable; absent for internal
  error?: string;
}

export interface ForcePushRecord {
  date: string;
  before: string | null;
  after: string | null;
  main_at_push?: string | null;
  already_contains_main?: boolean;
  conflicts: string[] | null;
  error?: string;
}

export interface PullRequestRecord {
  number: number;
  title: string;
  state: "OPEN" | "MERGED" | "CLOSED";
  baseRefName: string;
  headRefName: string;
  headRefOid: string;
  mergedAt: string | null;
  createdAt: string;
  closedAt?: string | null; // absent from the Python run's records and earlier runs
  commit_count: number;
  merges: MergeRecord[];
  force_pushes: ForcePushRecord[];
  open_conflicts_now?: string[] | null; // OPEN only
  open_behind_base?: boolean; // OPEN only, when the head was fetched
}

/** A whole run's raw records. A file without `measured_at`/`selection` is the Python run. */
export interface RawRun {
  since: string; // as given: YYYY-MM-DD or an RFC 3339 instant
  until: string; // as given; a run with no --until writes measured_at's instant here
  origin_main: string;
  prs: PullRequestRecord[];
  measured_at?: string; // RFC 3339
  selection?: "updated-since";
}

/** GitHub's pull request as the GraphQL query returns it, commits paged together. */
export interface GraphCommit {
  oid: string;
  messageHeadline: string;
  committedDate: string;
  parents: { nodes: Array<{ oid: string }> };
}
export interface GraphForcePush {
  createdAt: string;
  beforeCommit: { oid: string } | null;
  afterCommit: { oid: string } | null;
}
export interface GraphPullRequest {
  number: number;
  title: string;
  state: "OPEN" | "MERGED" | "CLOSED";
  baseRefName: string;
  headRefName: string;
  headRefOid: string;
  mergedAt: string | null;
  createdAt: string;
  closedAt: string | null;
  commits: GraphCommit[];
  forcePushes: GraphForcePush[];
}

export interface FixSpec {
  glob: string;
  commit: string; // full oid once resolved
}
/**
 * A conflicting forward merge of the combined section, as counted against the `--fixed` spec that
 * takes some of its conflicted paths (first matching glob wins per path): one per merge and glob.
 */
export interface FixPair {
  oid: string;
  glob: string;
  commit: string; // the spec's fix, a full oid
  branch_parent?: string;
  upstream_parent?: string;
  paths: string[]; // the merge's conflicted paths the glob takes, in the merge's order
}
/** merge oid -> FixSpec glob -> state, for the pairs `--fixed` counted. */
export type FixStates = Map<string, Map<string, FixState>>;

/** A usage error: bad flag, bad window, unreadable raw records, bad --fixed commit. Exit 2. */
export class UsageError extends Error {}
