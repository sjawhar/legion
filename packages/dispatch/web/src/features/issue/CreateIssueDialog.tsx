import { ISSUE_TITLE_MAX } from "@legion/contracts";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { type FormEvent, type ReactNode, useRef, useState } from "react";
import { useLocation, useNavigate } from "react-router-dom";

import { ApiError, api, type WriteBounds } from "../../api/client";
import { projectsQuery } from "../../api/queries";
import type { CreateIssueInput, Issue } from "../../api/types";
import { QueryError } from "../../components/QueryError";
import { submitOnModifiedEnter } from "../../hooks/submitOnModifiedEnter";
import { useSubmitGuard } from "../../hooks/useSubmitGuard";
import {
  backdrop50,
  borderDefault,
  card,
  inputClasses,
  primaryButtonBg,
  primaryButtonDisabled,
  primaryButtonEnabledHoverBg,
  secondaryButtonBorder,
  secondaryButtonHoverBorder,
  secondaryButtonText,
  textMutedOnSurface,
  textPrimaryOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { buildIssuePath, routeProjectOf } from "../refs/routes";
import { useDialog } from "../shell/useDialog";
import { LandedWithoutAnswer } from "../writes/LandedWithoutAnswer";
import { useKeyedWrite } from "../writes/useKeyedWrite";
import { RelatedItems } from "./RelatedItems";

const createIssue = (input: CreateIssueInput, bounds: WriteBounds): Promise<Issue> =>
  api.createIssue(input, bounds);

/**
 * The `c` shortcut's minimal create-issue dialog: project (preselecting the route's project),
 * title, and an optional first spec line. Mounted only while open, so its draft and error state
 * start fresh each time and `useDialog` restores focus to the caller on close. A server
 * `POSSIBLE_DUPLICATE` answer is shown with its candidate and can be overridden with
 * **Create anyway** (`force`), the same path an agent takes. Success opens the new issue. While
 * a person types a title, the dialog lists the related items the check finds for it, so a
 * duplicate is seen before it is filed.
 */
export function CreateIssueDialog({
  onClose,
  createIssue: create = createIssue,
}: {
  onClose: () => void;
  /** Write seam for tests; defaults to the real API. */
  createIssue?: (input: CreateIssueInput, bounds: WriteBounds) => Promise<Issue>;
}): ReactNode {
  const location = useLocation();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const titleRef = useRef<HTMLInputElement>(null);
  const dialog = useDialog<HTMLFormElement>({ initialFocusRef: titleRef, onClose, open: true });
  const projects = useQuery(projectsQuery());
  const routeProject = routeProjectOf(location.pathname);
  const [project, setProject] = useState<string | undefined>(undefined);
  const [title, setTitle] = useState("");
  const [spec, setSpec] = useState("");
  const submitGuard = useSubmitGuard();
  // The dialog shows a refusal itself, beside the draft it was refused over.
  const mutation = useKeyedWrite({
    inline: true,
    label: (input: CreateIssueInput) => `New issue “${input.title}”`,
    onSettled: () => submitGuard.release(),
    write: create,
    onSuccess: (issue) => {
      void queryClient.invalidateQueries({ queryKey: ["issues"] });
      void queryClient.invalidateQueries({ queryKey: projectsQuery().queryKey });
      if (issue !== null) {
        onClose();
        navigate(buildIssuePath({ key: issue.key, kind: "issue" }));
        return;
      }
      // Made, with no answer to open: the draft goes, so a second press makes nothing, and the
      // dialog says what was made (`mutation.unanswered`).
      setTitle("");
      setSpec("");
    },
  });

  const available = projects.data ?? [];
  const selectedProject =
    project ??
    available.find((candidate) => candidate.key === routeProject)?.key ??
    available[0]?.key ??
    "";
  const trimmedTitle = title.trim();
  const trimmedSpec = spec.trim();
  const canSubmit = selectedProject !== "" && trimmedTitle !== "" && !mutation.isPending;
  const submit = (force: boolean) =>
    submitGuard.guard(() =>
      mutation.mutate({
        project: selectedProject,
        title: trimmedTitle,
        ...(trimmedSpec === "" ? {} : { spec: trimmedSpec }),
        ...(force ? { force } : {}),
      })
    );
  const handleSubmit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (canSubmit) {
      submit(false);
    }
  };
  const duplicate =
    mutation.error instanceof ApiError && mutation.error.code === "POSSIBLE_DUPLICATE"
      ? mutation.error
      : undefined;

  return (
    <>
      <div aria-hidden="true" className={`fixed inset-0 z-40 ${backdrop50}`} onClick={onClose} />
      <div className="pointer-events-none fixed inset-0 z-50 flex items-start justify-center xl:p-4 xl:pt-20">
        {/* The dialog scrolls: the related section grows with what the check finds, and below
            `xl` the dialog is a full-bleed sheet, so without this the Create row - and the
            refusal's Create anyway with it - paints past the bottom edge of a phone window with
            nothing to scroll. The cap at `xl` leaves the wrapper's own `p-4 pt-20` outside it. */}
        <form
          aria-label="Create issue"
          aria-modal="true"
          className={`pointer-events-auto max-h-[100dvh] w-full space-y-3 overflow-y-auto border p-4 shadow-2xl xl:max-h-[calc(100dvh-6rem)] xl:max-w-lg xl:rounded-lg ${card} ${borderDefault}`}
          onSubmit={handleSubmit}
          ref={dialog.containerRef}
          role="dialog"
        >
          <h2 className={`text-base font-semibold ${textPrimaryOnSurface}`}>Create issue</h2>
          <label className={`block text-sm font-medium ${textSecondaryOnSurface}`}>
            Project
            <select
              className={`mt-1 block min-h-11 w-full rounded-lg border px-3 py-2 text-sm font-normal outline-none ${inputClasses(true)} ${textPrimaryOnSurface}`}
              disabled={mutation.isPending || available.length === 0}
              onChange={(event) => setProject(event.target.value)}
              value={selectedProject}
            >
              {available.map((candidate) => (
                <option key={candidate.key} value={candidate.key}>
                  {candidate.key} · {candidate.name}
                </option>
              ))}
            </select>
          </label>
          {projects.isSuccess && available.length === 0 ? (
            <p className={`text-sm ${textMutedOnSurface}`}>
              No projects yet — create one under Settings first.
            </p>
          ) : null}
          {projects.isError ? (
            <QueryError
              message="Could not load projects."
              onRetry={() => void projects.refetch()}
              retrying={projects.isFetching}
            />
          ) : null}
          <label className={`block text-sm font-medium ${textSecondaryOnSurface}`}>
            Title
            <input
              className={`mt-1 block min-h-11 w-full rounded-lg border px-3 py-2 text-sm font-normal outline-none ${inputClasses(true)}`}
              disabled={mutation.isPending}
              maxLength={ISSUE_TITLE_MAX}
              onChange={(event) => {
                mutation.forgetUnanswered();
                setTitle(event.target.value);
              }}
              ref={titleRef}
              type="text"
              value={title}
            />
          </label>
          <label className={`block text-sm font-medium ${textSecondaryOnSurface}`}>
            Spec <span className={`font-normal ${textMutedOnSurface}`}>(optional)</span>
            <textarea
              className={`mt-1 block w-full rounded-lg border px-3 py-2 text-sm font-normal outline-none ${inputClasses(true)}`}
              disabled={mutation.isPending}
              onChange={(event) => setSpec(event.target.value)}
              onKeyDown={(event) => submitOnModifiedEnter(event)}
              rows={3}
              value={spec}
            />
          </label>
          <RelatedItems project={selectedProject} title={title} />
          {mutation.unanswered === undefined ? null : (
            <LandedWithoutAnswer what={`The issue “${mutation.unanswered.variables.title}”`} />
          )}
          {mutation.isError ? (
            duplicate === undefined ? (
              <QueryError
                message={
                  mutation.error instanceof ApiError
                    ? mutation.error.message
                    : "Could not create the issue."
                }
                onRetry={() => submitGuard.retryLast(mutation)}
                retrying={mutation.isPending}
              />
            ) : (
              <p className={`text-sm ${textSecondaryOnSurface}`} role="alert">
                {duplicate.message}
              </p>
            )
          ) : null}
          <div className="flex items-center justify-end gap-2">
            <button
              className={`min-h-11 rounded-lg border px-3 py-2 text-sm font-medium ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder}`}
              onClick={onClose}
              type="button"
            >
              Cancel
            </button>
            {duplicate === undefined ? null : (
              <button
                className={`min-h-11 rounded-lg border px-3 py-2 text-sm font-medium ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder}`}
                disabled={!canSubmit}
                onClick={() => submit(true)}
                type="button"
              >
                Create anyway
              </button>
            )}
            <button
              className={`min-h-11 rounded-lg px-3 py-2 text-sm font-semibold ${primaryButtonBg} ${primaryButtonEnabledHoverBg} ${primaryButtonDisabled}`}
              disabled={!canSubmit}
              type="submit"
            >
              {mutation.isPending ? "Creating…" : "Create"}
            </button>
          </div>
        </form>
      </div>
    </>
  );
}
