import { urgencyWeights } from "../api";
import { escapeHtml, timeAgo } from "../html";
import type { SidebarEntry, SidebarFilters, Thread } from "../types";

function keyOf(thread: Thread): string {
  return `${thread.repo}#${thread.number}`;
}

// hasParentInList tells us whether this thread sits under another thread
// in the loaded set (sub-issues only exist same-repo). We render the parent
// pointer as a breadcrumb but never use it to hide the row — every thread
// that needs attention is its own first-class entry.
function hasParentInList(thread: Thread, knownKeys: Set<string>): boolean {
  if (thread.parentNumber === thread.number) return false;
  return knownKeys.has(`${thread.repo}#${thread.parentNumber}`);
}

function subThreadCount(thread: Thread, threads: Thread[]): number {
  return threads.filter(
    (candidate) => candidate.repo === thread.repo && candidate.parentNumber === thread.number
  ).length;
}

function searchText(thread: Thread, threads: Thread[]): string {
  const children = threads
    .filter(
      (candidate) => candidate.repo === thread.repo && candidate.parentNumber === thread.number
    )
    .map((candidate) => `${candidate.title} ${candidate.body}`)
    .join(" ");
  return `${thread.repo} ${thread.title} ${thread.body} ${children}`.toLowerCase();
}

export function visibleSidebarThreads(threads: Thread[], filters: SidebarFilters): SidebarEntry[] {
  const query = filters.search.trim().toLowerCase();
  const knownKeys = new Set(threads.map(keyOf));
  return threads
    .filter((thread) => {
      if (filters.status !== "all" && thread.state.toLowerCase() !== filters.status) return false;
      if (filters.urgency !== "all" && thread.urgency !== filters.urgency) return false;
      if (!filters.showAddressed && isAddressed(thread, filters.addressed)) return false;
      if (query && !searchText(thread, threads).includes(query)) return false;
      return true;
    })
    .sort((a, b) => {
      const aBlocking = a.state === "OPEN" && a.urgency === "blocking";
      const bBlocking = b.state === "OPEN" && b.urgency === "blocking";
      if (aBlocking !== bBlocking) return aBlocking ? -1 : 1;
      const urgencyDelta = urgencyWeights[b.urgency] - urgencyWeights[a.urgency];
      if (urgencyDelta !== 0) return urgencyDelta;
      return new Date(b.updatedAt).getTime() - new Date(a.updatedAt).getTime();
    })
    .map((thread) => ({
      thread,
      groupNumber: thread.parentNumber,
      subThreadCount: subThreadCount(thread, threads),
      parentInList: hasParentInList(thread, knownKeys),
      addressed: isAddressed(thread, filters.addressed),
    }));
}

// isAddressed returns true when the user has marked the thread as no longer
// needing attention AND no new activity has happened since. The check uses
// timestamps: if thread.updatedAt has advanced past the stored marker, the
// thread resurfaces automatically.
function isAddressed(thread: Thread, addressed: Record<string, string> | undefined): boolean {
  if (!addressed) return false;
  const marker = addressed[`${thread.repo}#${thread.number}`];
  if (!marker) return false;
  return new Date(thread.updatedAt).getTime() <= new Date(marker).getTime();
}

function renderFilterPill(kind: "status" | "urgency", value: string, current: string): string {
  const active = value === current ? " active" : "";
  return `<button type="button" class="pill${active}" data-filter="${kind}" data-value="${escapeHtml(
    value
  )}">${escapeHtml(value)}</button>`;
}

export function renderSidebar(threads: Thread[], filters: SidebarFilters): string {
  const entries = visibleSidebarThreads(threads, filters);
  // When the addressed filter is what's hiding the rest, surface a hint
  // so people aren't staring at an empty list without knowing why.
  const hiddenByAddressed = filters.showAddressed
    ? 0
    : visibleSidebarThreads(threads, { ...filters, showAddressed: true }).filter(
        (entry) => entry.addressed
      ).length;
  // Flat list. Every thread is one row, sorted by urgency + recency.
  // The parent relationship is shown as a breadcrumb on the row, not via
  // nesting / grouping — nesting-level shouldn't gate discovery.
  const rows = entries
    .map(({ thread, subThreadCount, parentInList, addressed }) => {
      const key = keyOf(thread);
      const selected = filters.selectedKey === key ? " selected" : "";
      const resolved = thread.state === "CLOSED" ? " resolved" : "";
      const blocking = thread.urgency === "blocking" ? " blocking" : "";
      const highlighted = filters.highlightedKeys?.has(key) ? " live-highlight" : "";
      const addressedCls = addressed ? " addressed" : "";
      const parentCrumb = parentInList
        ? `<span class="thread-parent" title="sub-thread of #${thread.parentNumber}">↳ #${thread.parentNumber}</span>`
        : "";
      return `<button type="button" class="thread-row${selected}${resolved}${blocking}${highlighted}${addressedCls}" data-thread-repo="${escapeHtml(thread.repo)}" data-thread-number="${thread.number}">
        <span class="thread-row-top">
          <span class="urgency-dot urgency-${thread.urgency}" title="${thread.urgency}"></span>
          <span class="thread-title">${escapeHtml(thread.title)}</span>
          <span class="thread-time">${escapeHtml(timeAgo(thread.updatedAt))}</span>
        </span>
        <span class="thread-meta">
          <span class="thread-repo" title="${escapeHtml(thread.repo)}">${escapeHtml(thread.repo)}</span>
          <span class="thread-number">#${thread.number}</span>
          ${parentCrumb}
          ${thread.state === "CLOSED" ? '<span class="badge state-badge state-closed">closed</span>' : ""}
          ${addressed ? '<span class="badge state-badge state-addressed">addressed</span>' : ""}
          ${subThreadCount > 0 ? `<span class="subthread-count">${subThreadCount} sub</span>` : ""}
        </span>
      </button>`;
    })
    .join("");

  return `<aside class="dispatch-sidebar" aria-label="Dispatch threads">
    <div class="sidebar-controls">
      <input id="search-input" type="search" value="${escapeHtml(filters.search)}" placeholder="Search threads" />
      <div class="filter-row" aria-label="Status filter">
        <span>Status</span>${["all", "open", "closed"]
          .map((value) => renderFilterPill("status", value, filters.status))
          .join("")}
      </div>
      <div class="filter-row" aria-label="Urgency filter">
        <span>Urgency</span>${["all", "blocking", "high", "med", "low"]
          .map((value) => renderFilterPill("urgency", value, filters.urgency))
          .join("")}
      </div>
      <div class="filter-row" aria-label="Addressed filter">
        <span>Addressed</span>
        <button type="button" class="pill${filters.showAddressed ? "" : " active"}" data-toggle="hide-addressed">hide</button>
        <button type="button" class="pill${filters.showAddressed ? " active" : ""}" data-toggle="show-addressed">show</button>
      </div>
    </div>
    <div id="thread-list" class="thread-list">${
      rows ||
      (hiddenByAddressed > 0
        ? `<div class="empty-state">No threads match. <button type="button" class="link-button" data-toggle="show-addressed">${hiddenByAddressed} addressed thread${hiddenByAddressed === 1 ? "" : "s"} hidden — Show addressed</button></div>`
        : `<div class="empty-state">No threads match.</div>`)
    }</div>
  </aside>`;
}
