import { type ReactNode, useState } from "react";

import type { DeliveryTimelineResponse } from "../../api/types";
import { MultiSelect } from "../../components/MultiSelect";
import {
  borderStrong,
  inputClasses,
  surfaceBg,
  textMutedOnCanvas,
  textPrimaryOnSurface,
} from "../../theme/classes";
import {
  DEPLOYED_LABELS,
  type DeployedFacet,
  FACET_PARAMS,
  type FacetKey,
  type Filters,
  PLACEHOLDER_LABELS,
  REWORK_LABELS,
  type ReworkFacet,
} from "./lib/facets";

type ListFacet = Exclude<FacetKey, "rework" | "deployed">;

const LIST_FACETS: { key: ListFacet; label: string }[] = [
  { key: "repo", label: "Repository" },
  { key: "parentAgent", label: "Parent agent" },
  { key: "session", label: "Session" },
  { key: "issue", label: "Dispatch issue" },
  { key: "priority", label: "Priority" },
  { key: "component", label: "Component" },
  { key: "author", label: "Author" },
];

const REWORK_VALUES: ReworkFacet[] = ["value", "rework"];
const DEPLOYED_VALUES: DeployedFacet[] = ["deployed", "waiting", "not_tracked"];

const facetLabelClass = `mb-1 block text-xs tracking-wide uppercase ${textMutedOnCanvas}`;
const triggerClass = `flex h-7 w-full items-center justify-between rounded border px-2 text-left text-xs ${borderStrong} ${surfaceBg} ${textPrimaryOnSurface}`;

/** One facet's searchable checkbox list: "Any <facet>" until something is picked, each value with
 *  its count, sorted by count. A facet with no values in the window and nothing picked is not
 *  shown. */
function Facet({
  label,
  values,
  counts,
  selected,
  valueLabel,
  open,
  onOpenChange,
  onChange,
}: {
  label: string;
  values: readonly string[];
  counts: Readonly<Record<string, number>>;
  selected: readonly string[];
  valueLabel: (value: string) => string;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onChange: (next: string[]) => void;
}): ReactNode {
  if (values.length === 0 && selected.length === 0) return null;
  const options = [...new Set([...selected, ...values])];
  const state =
    selected.length === 0 ? `Any ${label.toLowerCase()}` : `${selected.length} selected`;
  const countOf = (value: string) => counts[value] ?? 0;
  return (
    <div>
      <span className={facetLabelClass}>{label}</span>
      <MultiSelect
        emptyMessage="No matches."
        label={label}
        onChange={onChange}
        onOpenChange={onOpenChange}
        open={open}
        optionDetail={(value) => String(countOf(value))}
        optionDetailLabel={(value) => `${countOf(value)} PR${countOf(value) === 1 ? "" : "s"}`}
        optionLabel={valueLabel}
        options={options}
        searchLabel={`Search ${label.toLowerCase()}\u2026`}
        selected={selected}
        triggerAriaLabel={`${label}: ${state}`}
        triggerClassName={triggerClass}
      >
        <span className="truncate">{state}</span>
      </MultiSelect>
    </div>
  );
}

/** The delivery timeline's facet column: the search, Rework and Deployed side by side, then one
 *  facet per list (repository, parent agent, session, Dispatch issue, priority, component,
 *  author), each counted by the server with every other facet applied and its own ignored. All
 *  combinable; the caller keeps them in the URL. */
export function FacetPanel({
  data,
  filters,
  onChange,
}: {
  data: DeliveryTimelineResponse;
  filters: Filters;
  onChange: (next: Filters) => void;
}): ReactNode {
  const [openFacet, setOpenFacet] = useState<FacetKey | undefined>(undefined);
  const countsOf = (key: FacetKey) => data.facet_counts[FACET_PARAMS[key]];
  const byCount = (key: FacetKey) => {
    const counts = countsOf(key);
    return Object.keys(counts).sort((a, b) => (counts[b] ?? 0) - (counts[a] ?? 0));
  };
  // How many sub-components each component has in the window's projects: selecting a parent
  // selects them too (the server expands it), and the prototype lists components flat, so the
  // option says so.
  const subComponents = new Map<string, number>();
  for (const component of Object.values(data.components)) {
    const seen = new Set<string>();
    for (let parent = component.parent; parent !== null && !seen.has(parent); ) {
      seen.add(parent);
      subComponents.set(parent, (subComponents.get(parent) ?? 0) + 1);
      parent = data.components[parent]?.parent ?? null;
    }
  }
  const valueLabel = (key: FacetKey, value: string): string => {
    const placeholder = PLACEHOLDER_LABELS[value];
    if (placeholder !== undefined) return placeholder;
    if (key === "issue") return `${value} \u2014 ${data.issue_titles[value] ?? ""}`;
    if (key === "component") {
      const title = data.components[value]?.title ?? value;
      const children = subComponents.get(value) ?? 0;
      if (children === 0) return title;
      return `${title} (includes ${children} sub-component${children === 1 ? "" : "s"})`;
    }
    return value;
  };
  const facetProps = (key: FacetKey) => ({
    counts: countsOf(key),
    onChange: (next: string[]) => onChange({ ...filters, [key]: next }),
    onOpenChange: (open: boolean) => setOpenFacet(open ? key : undefined),
    open: openFacet === key,
    selected: filters[key],
  });

  return (
    <div className="flex flex-col gap-4 text-sm">
      <div>
        <label className={facetLabelClass} htmlFor="facet-search">
          Search
        </label>
        <input
          className={`h-8 w-full rounded border px-2 text-sm ${inputClasses(false)}`}
          id="facet-search"
          onChange={(event) => onChange({ ...filters, search: event.target.value })}
          placeholder="Title or PR id"
          type="text"
          value={filters.search}
        />
      </div>
      <div className="flex gap-4">
        <div className="min-w-0 flex-1">
          <Facet
            {...facetProps("rework")}
            label="Rework"
            valueLabel={(value) => REWORK_LABELS[value as ReworkFacet] ?? value}
            values={REWORK_VALUES}
          />
        </div>
        <div className="min-w-0 flex-1">
          <Facet
            {...facetProps("deployed")}
            label="Deployed"
            valueLabel={(value) => DEPLOYED_LABELS[value as DeployedFacet] ?? value}
            values={DEPLOYED_VALUES}
          />
        </div>
      </div>
      {LIST_FACETS.map(({ key, label }) => (
        <Facet
          {...facetProps(key)}
          key={key}
          label={label}
          valueLabel={(value) => valueLabel(key, value)}
          values={byCount(key)}
        />
      ))}
    </div>
  );
}
