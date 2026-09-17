import type {
  ArchitectureTree,
  ArchitectureTreeComponent,
  ArchitectureTreeIssue,
  ArchitectureTreeIssueRef,
  ArchitectureTreeSource,
  IssueComponentsInput,
} from "../../api/types";
import { isIssueStatus, issueStatuses } from "../project/board-model";

/** The importer's ticker runs every five minutes; a source unchecked for two ticks is stale. */
export const STALE_AFTER_MS = 10 * 60_000;

/**
 * What a component's fill bar says (spec item 4), over `done`/`total` only — nothing from CI or
 * pull requests changes it (item 5). External components are outside the rule: they are never
 * attachable, so they render bar-less with an `external` badge.
 * - `external`: an external component.
 * - `no-work`: `total == 0` — a dashed gray track and `No tracked work`.
 * - `untouched`: work exists, none done — a solid gray track.
 * - `partial`: some done — gray track, green fill.
 * - `done`: everything done, and every non-external descendant has work — the whole bar green.
 * - `done-with-gaps`: everything counted is done but a non-external descendant has no work —
 *   the partial look plus a `contains components with no tracked work` badge.
 */
export type ComponentTone =
  | "external"
  | "no-work"
  | "untouched"
  | "partial"
  | "done"
  | "done-with-gaps";

export type ComponentIndex = ReadonlyMap<string, ArchitectureTreeComponent>;

export function indexComponents(tree: Pick<ArchitectureTree, "components">): ComponentIndex {
  return new Map(tree.components.map((component) => [component.id, component]));
}

/** The components whose `parent` is `parent` (`null` for the roots), in the server's order. */
export function childComponents(
  components: readonly ArchitectureTreeComponent[],
  parent: string | null
): ArchitectureTreeComponent[] {
  return components.filter((component) => component.parent === parent);
}

/** Every component below `id`, transitively over `parent`. */
export function descendantComponents(
  components: readonly ArchitectureTreeComponent[],
  id: string
): ArchitectureTreeComponent[] {
  const byParent = new Map<string | null, ArchitectureTreeComponent[]>();
  for (const component of components) {
    const siblings = byParent.get(component.parent);
    if (siblings === undefined) {
      byParent.set(component.parent, [component]);
    } else {
      siblings.push(component);
    }
  }
  // The same breadth-first walk as shifting a queue, with a cursor instead of the shift.
  const result: ArchitectureTreeComponent[] = [];
  const queue = [id];
  for (let cursor = 0; cursor < queue.length; cursor += 1) {
    for (const child of byParent.get(queue[cursor] as string) ?? []) {
      result.push(child);
      queue.push(child.id);
    }
  }
  return result;
}

export function componentTone(
  component: ArchitectureTreeComponent,
  components: readonly ArchitectureTreeComponent[]
): ComponentTone {
  if (component.external) {
    return "external";
  }
  if (component.total === 0) {
    return "no-work";
  }
  if (component.done === 0) {
    return "untouched";
  }
  if (component.done < component.total) {
    return "partial";
  }
  const gaps = descendantComponents(components, component.id).some(
    (descendant) => !descendant.external && descendant.total === 0
  );
  return gaps ? "done-with-gaps" : "done";
}

/** The chain from a root down to `id`, `id` last; empty when `id` is not a component. */
export function componentPath(index: ComponentIndex, id: string): ArchitectureTreeComponent[] {
  const path: ArchitectureTreeComponent[] = [];
  let current = index.get(id);
  // The importer rejects parent cycles, so the walk ends at a root; the length guard only keeps
  // a malformed cache from spinning.
  while (current !== undefined && path.length <= index.size) {
    path.unshift(current);
    current = current.parent === null ? undefined : index.get(current.parent);
  }
  return path;
}

/** How the header's source line reads the sync state (spec item 6). */
export type SourceState = "never-synced" | "stale" | "fresh";

export function sourceState(source: ArchitectureTreeSource, now: number): SourceState {
  if (source.last_commit === null || source.last_sync_at === null) {
    return "never-synced";
  }
  return now - new Date(source.last_sync_at).getTime() > STALE_AFTER_MS ? "stale" : "fresh";
}

/** Whether every row wears the hatched outline: the last import failed, or the source is stale. */
export function modelDistrusted(source: ArchitectureTreeSource, now: number): boolean {
  return source.last_error !== null || sourceState(source, now) === "stale";
}

function statusRank(status: string): number {
  return isIssueStatus(status) ? issueStatuses.indexOf(status) : issueStatuses.length;
}

/** A component's Work section: unfinished issues first, by lifecycle status then newest
 *  activity; the done issues apart, newest first, for the `Show N done` disclosure. */
export function splitWork(issues: readonly ArchitectureTreeIssue[]): {
  open: ArchitectureTreeIssue[];
  done: ArchitectureTreeIssue[];
} {
  const byActivity = (left: ArchitectureTreeIssue, right: ArchitectureTreeIssue) =>
    right.updated_at.localeCompare(left.updated_at);
  const open = issues
    .filter((issue) => issue.status !== "done")
    .sort(
      (left, right) => statusRank(left.status) - statusRank(right.status) || byActivity(left, right)
    );
  const done = issues.filter((issue) => issue.status === "done").sort(byActivity);
  return { done, open };
}

/** Where a child issue of an expanded work row lives (spec item 9): its own row in this
 *  component, a row under another component (`elsewhere`), the `not_architectural` list, the
 *  `unassigned` list, or nowhere the tree counts (a retired-only link). */
export type ChildPlacement =
  | { kind: "here"; issue: ArchitectureTreeIssue }
  | { kind: "elsewhere"; component: string }
  | { kind: "not-architectural" }
  | { kind: "unassigned" }
  | { kind: "uncounted" };

export function placeChild(tree: ArchitectureTree, component: string, key: string): ChildPlacement {
  const here = tree.components
    .find((candidate) => candidate.id === component)
    ?.issues.find((issue) => issue.key === key);
  if (here !== undefined) {
    return { issue: here, kind: "here" };
  }
  for (const candidate of tree.components) {
    if (candidate.issues.some((issue) => issue.key === key)) {
      return { component: candidate.id, kind: "elsewhere" };
    }
  }
  if (tree.not_architectural.some((issue) => issue.key === key)) {
    return { kind: "not-architectural" };
  }
  if (tree.unassigned.some((issue) => issue.key === key)) {
    return { kind: "unassigned" };
  }
  return { kind: "uncounted" };
}

/** The `sessionStorage` key holding which work rows are expanded in a component's details, so
 *  Back from an issue page restores them (spec item 10). */
export function expansionStorageKey(project: string, component: string): string {
  return `dispatch.architecture.expanded:${project}:${component}`;
}

type Mutable<T> = { -readonly [K in keyof T]: T[K] };
type PredictedComponent = Mutable<Omit<ArchitectureTreeComponent, "issues">> & {
  issues: ArchitectureTreeIssue[];
};

/**
 * The tree as it will read once a component write on `issue` lands, for the optimistic cache
 * write: the issue leaves every list and row it is in (its counts with it), then joins the
 * lists the write names — `explicit` adds a `direct` row to each named component and a
 * `contained` row to each of their ancestors (once per ancestor), `none` adds a
 * `not_architectural` row. `inherit` places nothing: where the issue lands depends on its
 * ancestors' rows, which only the refetch knows. Descendants that inherit from the issue are
 * left to the refetch too.
 */
export function predictTree(
  tree: ArchitectureTree,
  issue: ArchitectureTreeIssueRef,
  input: IssueComponentsInput,
  now: string
): ArchitectureTree {
  const isDone = issue.status === "done" ? 1 : 0;
  const components: PredictedComponent[] = tree.components.map((component) => {
    const row = component.issues.find((candidate) => candidate.key === issue.key);
    if (row === undefined) {
      return { ...component, issues: [...component.issues] };
    }
    const own = row.attached === "direct" ? 1 : 0;
    return {
      ...component,
      done: component.done - isDone,
      issues: component.issues.filter((candidate) => candidate.key !== issue.key),
      own_done: component.own_done - isDone * own,
      own_total: component.own_total - own,
      total: component.total - 1,
    };
  });
  const index = new Map(components.map((component) => [component.id, component]));
  const notArchitectural = tree.not_architectural.filter((row) => row.key !== issue.key);
  const unassigned = tree.unassigned.filter((row) => row.key !== issue.key);
  const retired = tree.retired_links.filter((row) => row.key !== issue.key);

  const rowBase = {
    external_links: [],
    key: issue.key,
    parent: null,
    priority: null,
    status: issue.status,
    title: issue.title,
    updated_at: now,
  };
  if (input.mode === "explicit") {
    const counted = new Set<string>();
    const count = (component: PredictedComponent, row: ArchitectureTreeIssue) => {
      counted.add(component.id);
      component.issues.push(row);
      component.total += 1;
      component.done += isDone;
      if (row.attached === "direct") {
        component.own_total += 1;
        component.own_done += isDone;
      }
    };
    for (const id of input.ids ?? []) {
      const target = index.get(id);
      if (target === undefined || counted.has(id)) {
        continue;
      }
      count(target, { ...rowBase, attached: "direct" });
      let parent = target.parent === null ? undefined : index.get(target.parent);
      while (parent !== undefined && !counted.has(parent.id)) {
        count(parent, { ...rowBase, attached: "contained", via: id });
        parent = parent.parent === null ? undefined : index.get(parent.parent);
      }
    }
  } else if (input.mode === "none") {
    notArchitectural.push({
      inherited_from: null,
      key: issue.key,
      reason: input.reason ?? "",
      status: issue.status,
      title: issue.title,
    });
  }
  return {
    ...tree,
    components,
    not_architectural: notArchitectural,
    retired_links: retired,
    totals: {
      ...tree.totals,
      components_without_work: components.filter(
        (component) => !component.external && component.total === 0
      ).length,
      not_architectural: notArchitectural.length,
      retired_links: retired.length,
      unassigned: unassigned.length,
    },
    unassigned,
  };
}
