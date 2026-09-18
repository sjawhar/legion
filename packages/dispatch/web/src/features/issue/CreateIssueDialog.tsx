import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { type FormEvent, type ReactNode, useRef, useState } from "react";
import { useLocation, useNavigate } from "react-router-dom";

import { ApiError, api } from "../../api/client";
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
import { buildIssuePath, parseIssuePath, parseProjectPath } from "../refs/routes";
import { useDialog } from "../shell/useDialog";

const createIssue = (input: CreateIssueInput): Promise<Issue> => api.createIssue(input);

/**
 * The `c` shortcut's minimal create-issue dialog: project (preselecting the route's project),
 * title, and an optional first spec line. Mounted only while open, so its draft and error state
 * start fresh each time and `useDialog` restores focus to the caller on close. A server
 * `POSSIBLE_DUPLICATE` answer is shown with its candidate and can be overridden with
 * **Create anyway** (`force`), the same path an agent takes. Success opens the new issue.
 */
export function CreateIssueDialog({
  onClose,
  createIssue: create = createIssue,
}: {
  onClose: () => void;
  /** Write seam for tests; defaults to the real API. */
  createIssue?: (input: CreateIssueInput) => Promise<Issue>;
}): ReactNode {
  const location = useLocation();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const titleRef = useRef<HTMLInputElement>(null);
  const dialog = useDialog<HTMLFormElement>({ initialFocusRef: titleRef, onClose, open: true });
  const projects = useQuery(projectsQuery());
  // An issue key is `<PROJECT>-<n>` (routes.ts `issueKeyPattern`), so its project is the prefix.
  const routeProject =
    parseProjectPath(location.pathname)?.project ??
    parseIssuePath(location.pathname)?.key.replace(/-\d+$/, "");
  const [project, setProject] = useState<string | undefined>(undefined);
  const [title, setTitle] = useState("");
  const [spec, setSpec] = useState("");
  const submitGuard = useSubmitGuard();
  const mutation = useMutation({
    mutationFn: create,
    onSettled: () => submitGuard.release(),
    onSuccess: (issue) => {
      void queryClient.invalidateQueries({ queryKey: ["issues"] });
      void queryClient.invalidateQueries({ queryKey: projectsQuery().queryKey });
      onClose();
      navigate(buildIssuePath({ key: issue.key, kind: "issue" }));
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
        <form
          aria-label="Create issue"
          aria-modal="true"
          className={`pointer-events-auto w-full space-y-3 border p-4 shadow-2xl xl:max-w-lg xl:rounded-lg ${card} ${borderDefault}`}
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
              onChange={(event) => setTitle(event.target.value)}
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
