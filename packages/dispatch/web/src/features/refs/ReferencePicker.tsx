import { useQueries, useQuery } from "@tanstack/react-query";
import { type KeyboardEvent, type ReactNode, useState } from "react";

import { api } from "../../api/client";
import {
  backdrop40,
  card,
  disclosureButtonText,
  dismissButtonText,
  inputClasses,
  surfaceMutedHoverBg,
  textMutedOnSurface,
} from "../../theme/classes";
import { useDialog } from "../shell/useDialog";
import { buildDispatchReference } from "./routes";

export interface ReferencePickerProps {
  issueKey: string;
  onClose: () => void;
  onSelect: (reference: string) => void;
}

/**
 * The Ctrl+K reference picker the composer opens. It is a real dialog (Escape, backdrop
 * click, and a Close button all dismiss it) and loads an issue's artifacts only once that
 * issue's row is expanded, instead of fanning out one request per project issue up front.
 */
export function ReferencePicker({ issueKey, onClose, onSelect }: ReferencePickerProps): ReactNode {
  const [filter, setFilter] = useState("");
  const [expandedIssues, setExpandedIssues] = useState<Set<string>>(new Set());
  const dialog = useDialog<HTMLDivElement>({ onClose, open: true });
  const currentIssue = useQuery({
    queryKey: ["issue", issueKey],
    queryFn: () => api.getIssue(issueKey),
  });
  const issues = useQuery({
    enabled: currentIssue.data !== undefined,
    queryKey: ["issues", currentIssue.data?.project],
    queryFn: () => api.listIssues({ project: currentIssue.data?.project }),
  });
  const needle = filter.trim().toLowerCase();
  const filtered = (issues.data ?? []).filter(
    (issue) =>
      needle === "" ||
      issue.key.toLowerCase().includes(needle) ||
      issue.title.toLowerCase().includes(needle)
  );
  const artifactQueries = useQueries({
    queries: filtered.map((issue) => ({
      enabled: expandedIssues.has(issue.key),
      queryKey: ["artifacts", issue.key],
      queryFn: () => api.listArtifacts(issue.key),
    })),
  });

  return (
    <>
      <div aria-hidden="true" className={`fixed inset-0 z-40 ${backdrop40}`} onClick={onClose} />
      <div className="pointer-events-none fixed inset-0 z-40 flex items-start justify-center p-4 pt-20">
        <section
          aria-label="Reference picker"
          aria-modal="true"
          className={`pointer-events-auto max-h-[70vh] w-full max-w-md space-y-2 overflow-y-auto rounded-lg border p-3 shadow-xl ${card}`}
          ref={dialog.containerRef}
          role="dialog"
        >
          <div className="flex items-center justify-between gap-2">
            <input
              aria-label="Filter issues"
              className={`flex-1 rounded-lg border px-2 py-1 text-sm outline-none ${inputClasses(true)}`}
              onChange={(event) => setFilter(event.target.value)}
              onKeyDown={(event: KeyboardEvent<HTMLInputElement>) => {
                if (event.key !== "Enter") {
                  return;
                }
                // The filter sits inside the composer's <form>; without this the implicit
                // submit behavior of Enter-in-an-input would submit the draft instead of
                // picking a reference.
                event.preventDefault();
                const topMatch = filtered[0];
                if (topMatch !== undefined) {
                  onSelect(buildDispatchReference({ key: topMatch.key, kind: "issue" }));
                }
              }}
              placeholder="Filter issues…"
              type="text"
              value={filter}
            />
            <button className={`text-sm ${dismissButtonText}`} onClick={onClose} type="button">
              Close
            </button>
          </div>
          {issues.isPending ? (
            <p className={`text-sm ${textMutedOnSurface}`}>Loading issues…</p>
          ) : null}
          {filtered.map((issue, index) => {
            const expanded = expandedIssues.has(issue.key);
            return (
              <div className="space-y-1" key={issue.key}>
                <div className="flex items-center gap-1">
                  <button
                    aria-expanded={expanded}
                    aria-label={expanded ? `Collapse ${issue.key}` : `Expand ${issue.key}`}
                    className={`rounded px-1 ${disclosureButtonText}`}
                    onClick={() =>
                      setExpandedIssues((current) => {
                        const next = new Set(current);
                        if (next.has(issue.key)) {
                          next.delete(issue.key);
                        } else {
                          next.add(issue.key);
                        }
                        return next;
                      })
                    }
                    type="button"
                  >
                    {expanded ? "▾" : "▸"}
                  </button>
                  <button
                    className={`block flex-1 rounded px-2 py-1 text-left text-sm ${surfaceMutedHoverBg}`}
                    onClick={() =>
                      onSelect(buildDispatchReference({ key: issue.key, kind: "issue" }))
                    }
                    type="button"
                  >
                    {issue.key}: {issue.title}
                  </button>
                </div>
                {expanded && artifactQueries[index]?.isPending ? (
                  <p className={`px-2 text-xs ${textMutedOnSurface}`}>Loading artifacts…</p>
                ) : null}
                {(expanded ? (artifactQueries[index]?.data ?? []) : []).map((artifact) => (
                  <button
                    className={`ml-5 block w-[calc(100%-1.25rem)] rounded px-2 py-1 text-left text-sm ${surfaceMutedHoverBg}`}
                    key={artifact.id}
                    onClick={() =>
                      onSelect(
                        buildDispatchReference({
                          key: issue.key,
                          kind: "artifact",
                          slug: artifact.slug,
                        })
                      )
                    }
                    type="button"
                  >
                    {artifact.name}
                  </button>
                ))}
              </div>
            );
          })}
          {!issues.isPending && filtered.length === 0 ? (
            <p className={`px-2 text-sm ${textMutedOnSurface}`}>
              No issues match &quot;{filter}&quot;.
            </p>
          ) : null}
        </section>
      </div>
    </>
  );
}
