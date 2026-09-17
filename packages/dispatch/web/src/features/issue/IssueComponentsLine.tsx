import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useState } from "react";
import { Link } from "react-router-dom";

import { api } from "../../api/client";
import type { Issue } from "../../api/types";
import {
  badgeHigh,
  badgePrimary,
  dangerText,
  linkHoverText,
  linkText,
  textMutedOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { ComponentPicker } from "../architecture/ComponentPicker";
import { buildIssuePath } from "../refs/routes";
import { useIssueComponents } from "./useIssueComponents";

/**
 * The `Components:` line of the issue header's metadata rail: the issue's effective
 * attachment as chips. An explicit set lists its live component ids and marks each id a
 * re-import retired; `none` shows the reason; an issue no ancestor chain attached reads
 * `Not attached`. When the attachment is an ancestor's, that ancestor is named and linked.
 * The line is editable only when the project has an architecture source — the small
 * `["architecture-source", project]` lookup the project page shares, 404 SOURCE_NOT_FOUND
 * meaning read-only — through the same `ComponentPicker` the Architecture pane uses: a picked
 * set saves `explicit`, clearing every pick saves `inherit` (back to the ancestors'). The
 * component list itself (the whole tree, refetched on every issue event in the project) is
 * fetched only while the picker is open.
 */
export function IssueComponentsLine({
  issue,
}: {
  issue: Pick<Issue, "components" | "key" | "project" | "status" | "title">;
}): ReactNode {
  const { components } = issue;
  const source = useQuery({
    queryKey: ["architecture-source", issue.project],
    queryFn: () => api.getArchitectureSource(issue.project),
  });
  const [pickerOpen, setPickerOpen] = useState(false);
  const tree = useQuery({
    queryKey: ["architecture", issue.project],
    queryFn: () => api.getArchitecture(issue.project),
    enabled: pickerOpen,
  });
  const write = useIssueComponents(issue);
  const inheritedFrom =
    components.inherited_from === null ? null : (
      <span className={`shrink-0 text-xs ${textMutedOnSurface}`}>
        inherited from{" "}
        <Link
          className={`underline ${linkText} ${linkHoverText}`}
          to={buildIssuePath({ key: components.inherited_from, kind: "issue" })}
        >
          {components.inherited_from}
        </Link>
      </span>
    );
  return (
    <div
      className={`flex shrink-0 items-center gap-2 text-sm ${textSecondaryOnSurface}`}
      data-testid="issue-components"
    >
      <span className="font-medium">Components:</span>
      {components.mode === "inherit" ? (
        <span className={textMutedOnSurface}>Not attached</span>
      ) : components.mode === "none" ? (
        <span className={textMutedOnSurface}>None — {components.reason}</span>
      ) : (
        <>
          {components.ids.map((id) => (
            <span
              className={`inline-flex shrink-0 items-center rounded-full px-2 py-0.5 text-xs font-medium ${badgePrimary.bg} ${badgePrimary.text}`}
              key={id}
            >
              {id}
            </span>
          ))}
          {components.unknown.map((id) => (
            <span
              className={`inline-flex shrink-0 items-center rounded-full px-2 py-0.5 text-xs font-medium ${badgeHigh.bg} ${badgeHigh.text}`}
              key={id}
              title={`${id} is no longer in the project's architecture model`}
            >
              retired: {id}
            </span>
          ))}
        </>
      )}
      {inheritedFrom}
      {source.isSuccess ? (
        <ComponentPicker
          components={tree.data?.components ?? []}
          error={tree.error === null ? null : tree.error.message}
          loading={tree.data === undefined && tree.error === null}
          onOpenChange={setPickerOpen}
          onSave={(ids) =>
            void write.submit(ids.length === 0 ? { mode: "inherit" } : { ids, mode: "explicit" })
          }
          saving={write.pending}
          selected={
            components.mode === "explicit" && components.inherited_from === null
              ? components.ids
              : []
          }
          triggerAriaLabel={`Edit components of ${issue.key}`}
        >
          {components.mode === "explicit" && components.inherited_from === null
            ? "+"
            : "Set components"}
        </ComponentPicker>
      ) : null}
      {write.error === null ? null : (
        <span className={`shrink-0 text-xs ${dangerText}`} role="alert">
          {write.error}
        </span>
      )}
    </div>
  );
}
