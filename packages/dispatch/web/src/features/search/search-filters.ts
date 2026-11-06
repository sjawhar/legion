import { statusText } from "../project/board-model";

/**
 * What the bar's chips narrow a search by: one project key, one issue status, one author (a
 * person's login or a session id, which is what `GET /api/v1/search` matches an item's actor
 * against) and the day at or after which the item was last written, held as the date control's
 * own `YYYY-MM-DD`. Each holds what the person picked rather than a query string, so a chip
 * reads it back in their own words and removing one leaves the rest untouched.
 */
export interface SearchBarFilters {
  readonly project?: string;
  readonly status?: string;
  readonly author?: string;
  readonly updatedSince?: string;
}

/** Which of the four a chip is, and which control comes back when it is removed. */
export type SearchFilterName = "project" | "status" | "author" | "updatedSince";

export interface SearchFilterChip {
  readonly name: SearchFilterName;
  readonly label: string;
}

/** What one search sends: the parameters the chips narrow by, each absent when no chip set it,
 *  and `number_project`, the project of the page the bar was opened on, which narrows nothing
 *  (`packages/envoy/cmd/dispatch/README.md`, "Search"). */
export interface SearchRequest {
  readonly project?: string;
  readonly number_project?: string;
  readonly status?: string;
  readonly author?: string;
  readonly updated_since?: string;
}

/** The `Date` a `YYYY-MM-DD` names where the reader is, or `undefined` for anything the date
 *  control cannot have written. */
function pickedDay(day: string): Date | undefined {
  const [year, month, date] = day.split("-").map(Number);
  if (year === undefined || month === undefined || date === undefined) {
    return undefined;
  }
  const picked = new Date(year, month - 1, date);
  return Number.isNaN(picked.getTime()) ? undefined : picked;
}

/** The day as the reader writes it - `1 November`, and `1 November 2025` for a day of another
 *  year, since a year the reader is not in is part of what they chose. The Activity tab's day
 *  headings are formatted the same way (`features/activity/activity-model.ts`). A value the
 *  date control cannot have written reads as it stands. */
export function dayText(day: string, today: Date = new Date()): string {
  const picked = pickedDay(day);
  if (picked === undefined) {
    return day;
  }
  return new Intl.DateTimeFormat(undefined, {
    day: "numeric",
    month: "long",
    ...(picked.getFullYear() === today.getFullYear() ? {} : { year: "numeric" }),
  }).format(picked);
}

/** The chips the bar shows for these filters, in the order it shows them: each names the filter
 *  and the value in the words the person chose (`Project INFRA`), never the query string a
 *  search syntax would spell it as. */
export function searchFilterChips(
  filters: SearchBarFilters,
  today: Date = new Date()
): SearchFilterChip[] {
  const chips: SearchFilterChip[] = [];
  if (filters.project !== undefined) {
    chips.push({ label: `Project ${filters.project}`, name: "project" });
  }
  if (filters.status !== undefined) {
    chips.push({ label: `Status ${statusText(filters.status)}`, name: "status" });
  }
  if (filters.author !== undefined) {
    chips.push({ label: `Author ${filters.author}`, name: "author" });
  }
  if (filters.updatedSince !== undefined) {
    chips.push({
      label: `Updated since ${dayText(filters.updatedSince, today)}`,
      name: "updatedSince",
    });
  }
  return chips;
}

/** The parameters `api.search` carries: the route's own name for each chip, the day as the RFC
 *  3339 instant its midnight began where the reader is, which is what they mean by "since that
 *  day", and `numberProject`, the page's own project, which the route reads a bare issue number
 *  in and narrows nothing by. */
export function searchRequest(filters: SearchBarFilters, numberProject?: string): SearchRequest {
  const since = filters.updatedSince === undefined ? undefined : pickedDay(filters.updatedSince);
  return {
    ...(filters.project === undefined ? {} : { project: filters.project }),
    ...(numberProject === undefined ? {} : { number_project: numberProject }),
    ...(filters.status === undefined ? {} : { status: filters.status }),
    ...(filters.author === undefined ? {} : { author: filters.author }),
    ...(since === undefined ? {} : { updated_since: since.toISOString() }),
  };
}
