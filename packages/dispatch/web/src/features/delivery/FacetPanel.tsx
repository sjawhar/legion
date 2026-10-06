import { type ReactNode, useMemo, useState } from "react";

import type { DeliveryPR } from "../../api/types";
import { Chip } from "../../components/Chip";
import { MultiSelect } from "../../components/MultiSelect";
import { inputClasses, textSecondaryOnCanvas } from "../../theme/classes";
import {
  DEPLOYED_LABELS,
  type DeployedFacet,
  type Filters,
  NO_ISSUE,
  PLACEHOLDER_LABELS,
  parentAgentPlaceholder,
  REWORK_LABELS,
  type ReworkFacet,
} from "./lib/facets";

type MultiKey = "repo" | "parentAgent" | "session" | "issue" | "author";

const MULTI_FACETS: { key: MultiKey; label: string; searchLabel: string; emptyMessage: string }[] =
  [
    {
      key: "repo",
      label: "Repository",
      searchLabel: "Search repositories",
      emptyMessage: "No repositories yet.",
    },
    {
      key: "parentAgent",
      label: "Parent agent",
      searchLabel: "Search parent agents",
      emptyMessage: "No parent agents yet.",
    },
    {
      key: "session",
      label: "Session",
      searchLabel: "Search sessions",
      emptyMessage: "No sessions yet.",
    },
    {
      key: "issue",
      label: "Dispatch issue",
      searchLabel: "Search issues",
      emptyMessage: "No issues yet.",
    },
    {
      key: "author",
      label: "Author",
      searchLabel: "Search authors",
      emptyMessage: "No authors yet.",
    },
  ];

/** Every distinct value of one client-side facet among the window's PRs, for the MultiSelect's
 *  `options` list — this slice has no distinct-values endpoint, so the options a picker offers
 *  grow from what the current (unfiltered-by-this-facet) response actually contains; `onCreate`
 *  lets a caller add any other value by typing it (the server still validates/filters by it). */
function optionsFor(prs: readonly DeliveryPR[], key: MultiKey): string[] {
  const values = new Set<string>();
  for (const pr of prs) {
    switch (key) {
      case "repo":
        values.add(pr.repo);
        break;
      case "parentAgent":
        values.add(pr.parent_agent ?? parentAgentPlaceholder(pr));
        break;
      case "session":
        for (const s of pr.sessions) values.add(s);
        break;
      case "issue":
        values.add(pr.issue ?? NO_ISSUE);
        break;
      case "author":
        values.add(pr.author);
        break;
      default:
        break;
    }
  }
  return Array.from(values).sort((a, b) => a.localeCompare(b));
}

function labelFor(value: string): string {
  return PLACEHOLDER_LABELS[value] ?? value;
}

/** The delivery timeline's facet strip: the client-side facets (`filters.ts`'s module comment
 *  explains why priority/component are not here) plus `priority`/`component`, sent straight to
 *  the server since only it has the joined Dispatch issue data to filter by them. All combinable,
 *  all URL-persisted by the caller (`DeliveryPage`'s `useDeliveryFilters`). */
export function FacetPanel({
  prs,
  filters,
  onChange,
  priority,
  onPriorityChange,
  component,
  onComponentChange,
}: {
  prs: readonly DeliveryPR[];
  filters: Filters;
  onChange: (next: Filters) => void;
  priority: string[];
  onPriorityChange: (next: string[]) => void;
  component: string[];
  onComponentChange: (next: string[]) => void;
}): ReactNode {
  const [openPicker, setOpenPicker] = useState<MultiKey | "priority" | "component" | undefined>(
    undefined
  );
  const multiOptions = useMemo(
    () =>
      Object.fromEntries(MULTI_FACETS.map(({ key }) => [key, optionsFor(prs, key)])) as Record<
        MultiKey,
        string[]
      >,
    [prs]
  );
  const setMulti = (key: MultiKey, next: string[]) => onChange({ ...filters, [key]: next });
  const toggleRework = (value: ReworkFacet) =>
    onChange({
      ...filters,
      rework: filters.rework.includes(value)
        ? filters.rework.filter((v) => v !== value)
        : [...filters.rework, value],
    });
  const toggleDeployed = (value: DeployedFacet) =>
    onChange({
      ...filters,
      deployed: filters.deployed.includes(value)
        ? filters.deployed.filter((v) => v !== value)
        : [...filters.deployed, value],
    });

  return (
    <div className="flex flex-col gap-3 text-sm">
      <label className={`text-sm font-medium ${textSecondaryOnCanvas}`}>
        Search
        <input
          aria-label="Search merged PRs"
          className={`mt-1 block min-h-11 w-full max-w-xs rounded-lg px-3 py-2 text-sm font-normal ${inputClasses(true)}`}
          onChange={(event) => onChange({ ...filters, search: event.target.value })}
          placeholder="Title or PR id"
          type="search"
          value={filters.search}
        />
      </label>
      <div className="flex flex-wrap items-end gap-3">
        {MULTI_FACETS.map(({ key, label, searchLabel, emptyMessage }) => (
          <MultiSelect
            emptyMessage={emptyMessage}
            key={key}
            label={label}
            onChange={(next) => setMulti(key, next)}
            onCreate={(value) => setMulti(key, [...filters[key], value])}
            onOpenChange={(open) => setOpenPicker(open ? key : undefined)}
            open={openPicker === key}
            options={multiOptions[key]}
            optionLabel={labelFor}
            searchLabel={searchLabel}
            selected={filters[key]}
          />
        ))}
        <MultiSelect
          emptyMessage="Type a priority (e.g. P0)."
          label="Priority"
          onChange={onPriorityChange}
          onCreate={(value) => onPriorityChange([...priority, value])}
          onOpenChange={(open) => setOpenPicker(open ? "priority" : undefined)}
          open={openPicker === "priority"}
          options={[]}
          searchLabel="Add a priority"
          selected={priority}
        />
        <MultiSelect
          emptyMessage="Type a component id."
          label="Component"
          onChange={onComponentChange}
          onCreate={(value) => onComponentChange([...component, value])}
          onOpenChange={(open) => setOpenPicker(open ? "component" : undefined)}
          open={openPicker === "component"}
          options={[]}
          searchLabel="Add a component id"
          selected={component}
        />
      </div>
      <div className="flex flex-wrap gap-2">
        {(Object.keys(REWORK_LABELS) as ReworkFacet[]).map((value) => (
          <Chip
            key={value}
            onClick={() => toggleRework(value)}
            selected={filters.rework.includes(value)}
          >
            {REWORK_LABELS[value]}
          </Chip>
        ))}
        {(Object.keys(DEPLOYED_LABELS) as DeployedFacet[]).map((value) => (
          <Chip
            key={value}
            onClick={() => toggleDeployed(value)}
            selected={filters.deployed.includes(value)}
          >
            {DEPLOYED_LABELS[value]}
          </Chip>
        ))}
      </div>
    </div>
  );
}
