import { PROJECT_NAME_MAX } from "@legion/contracts";
import type { Mutation } from "@tanstack/react-query";
import { useMutation, useMutationState, useQuery, useQueryClient } from "@tanstack/react-query";
import { type FormEvent, type ReactNode, useState } from "react";

import { api, apiErrorMessage } from "../../api/client";
import { projectDefaultAssigneesQuery, projectsQuery, usersQuery } from "../../api/queries";
import type { CreateProjectInput, ProjectDefaultAssignee } from "../../api/types";
import { QueryError } from "../../components/QueryError";
import { useSubmitGuard } from "../../hooks/useSubmitGuard";
import {
  borderDefault,
  inputClasses,
  textMutedOnCanvas,
  textMutedOnSurface,
  textPrimaryOnCanvas,
  textPrimaryOnSurface,
  textSecondaryOnCanvas,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { Timestamp } from "../refs/Timestamp";
import {
  settingsFieldLabel,
  settingsMonoInput,
  settingsSubmitButton,
  settingsTableHead,
  settingsTableRow,
  settingsTableWrapper,
} from "./classes";

/** Lists native projects, lets a human create one, and carries each project's default assignee -
 * the person an issue created with nobody named lands on. The Settings route's repository
 * mapping form reads the same `["projects"]` query, so a project created here is
 * immediately selectable there without any explicit wiring between the two sections. */
export function ProjectsSection(): ReactNode {
  const queryClient = useQueryClient();
  const [projectKey, setProjectKey] = useState("");
  const [projectName, setProjectName] = useState("");
  const projectSubmitGuard = useSubmitGuard();
  const projects = useQuery(projectsQuery());
  const createProject = useMutation({
    mutationFn: (input: CreateProjectInput) => api.createProject(input),
    onSettled: () => projectSubmitGuard.release(),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: projectsQuery().queryKey });
      setProjectKey("");
      setProjectName("");
    },
  });
  const submitProject = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    projectSubmitGuard.guard(() => createProject.mutate({ key: projectKey, name: projectName }));
  };
  const defaults = useQuery(projectDefaultAssigneesQuery());
  // The people list is read only once a reader reaches for a picker, as `AssigneeControl` does
  // it, so opening Settings makes no request for it.
  const [wantUsers, setWantUsers] = useState(false);
  const users = useQuery({ ...usersQuery(), enabled: wantUsers });
  const defaultAssignees = new Map((defaults.data ?? []).map((row) => [row.project, row.assignee]));
  const logins = (users.data ?? []).map((user) => user.login);

  return (
    <section aria-labelledby="projects-heading">
      <h2 className={`text-xl font-semibold ${textPrimaryOnCanvas}`} id="projects-heading">
        Projects
      </h2>
      <p className={`mt-1 text-sm ${textSecondaryOnCanvas}`}>
        Native issues are grouped under a project key, such as CORE-1.
      </p>

      {projects.isPending ? <p className={`mt-6 ${textMutedOnCanvas}`}>Loading projects…</p> : null}
      {projects.isError ? (
        <div className="mt-6">
          <QueryError
            message="Couldn't load projects."
            onRetry={() => void projects.refetch()}
            retrying={projects.isFetching}
          />
        </div>
      ) : null}
      {projects.isSuccess ? (
        <>
          <div className={settingsTableWrapper}>
            <table className="w-full text-left text-sm">
              <thead className={settingsTableHead}>
                <tr>
                  <th className="px-4 py-3 font-semibold" scope="col">
                    Key
                  </th>
                  <th className="px-4 py-3 font-semibold" scope="col">
                    Name
                  </th>
                  <th className="px-4 py-3 font-semibold" scope="col">
                    Default assignee
                  </th>
                  <th className="px-4 py-3 font-semibold" scope="col">
                    Created
                  </th>
                </tr>
              </thead>
              <tbody>
                {projects.data.length === 0 ? (
                  <tr>
                    <td className={`px-4 py-5 ${textMutedOnSurface}`} colSpan={4}>
                      No projects yet.
                    </td>
                  </tr>
                ) : (
                  projects.data.map((candidate) => (
                    <tr className={settingsTableRow} key={candidate.key}>
                      <td className={`px-4 py-3 font-mono ${textPrimaryOnSurface}`}>
                        {candidate.key}
                      </td>
                      <td className={`px-4 py-3 ${textSecondaryOnSurface}`}>{candidate.name}</td>
                      <td className="px-4 py-3">
                        <DefaultAssigneeCell
                          logins={logins}
                          onReach={() => setWantUsers(true)}
                          project={candidate.key}
                          readFailed={defaults.isError}
                          stored={
                            defaults.data === undefined
                              ? undefined
                              : (defaultAssignees.get(candidate.key) ?? null)
                          }
                        />
                      </td>
                      <td className={`px-4 py-3 ${textMutedOnSurface}`}>
                        <Timestamp at={candidate.created_at} />
                      </td>
                    </tr>
                  ))
                )}
              </tbody>
            </table>
          </div>
          <p className={`mt-2 text-sm ${textSecondaryOnCanvas}`}>
            An issue created from a repository with no project mapping lands in the default project,
            so a default assignee set on that project also receives each such issue that is created
            with nobody named and with no person, token owner or assigned parent to take it.
          </p>
          {defaults.isError ? (
            <div className="mt-4">
              <QueryError
                message="Couldn't load the default assignees."
                onRetry={() => void defaults.refetch()}
                retrying={defaults.isFetching}
              />
            </div>
          ) : null}

          <form
            className={`mt-6 grid gap-4 rounded-xl border p-4 sm:grid-cols-[1fr_2fr_auto] sm:items-end ${borderDefault}`}
            onSubmit={submitProject}
          >
            <label className={settingsFieldLabel} htmlFor="project-key">
              Key
              <input
                className={settingsMonoInput}
                disabled={createProject.isPending}
                id="project-key"
                onChange={(event) => setProjectKey(event.target.value.trim().toUpperCase())}
                placeholder="CORE"
                required
                value={projectKey}
              />
            </label>
            <label className={settingsFieldLabel} htmlFor="project-name">
              Name
              <input
                className={`rounded-md px-3 py-2 ${inputClasses(false)} ${textPrimaryOnSurface}`}
                disabled={createProject.isPending}
                id="project-name"
                maxLength={PROJECT_NAME_MAX}
                onChange={(event) => setProjectName(event.target.value)}
                placeholder="Core"
                required
                value={projectName}
              />
            </label>
            <button
              className={settingsSubmitButton}
              disabled={createProject.isPending}
              type="submit"
            >
              New project
            </button>
          </form>
          {createProject.isError ? (
            <div className="mt-4">
              <QueryError
                message={apiErrorMessage(createProject.error, "Couldn't create the project.")}
                onRetry={() => projectSubmitGuard.retryLast(createProject)}
                retrying={createProject.isPending}
              />
            </div>
          ) : null}
        </>
      ) : null}
    </section>
  );
}

/** One project's default-assignee cell: the picker and, under it, the error of this row's own
 *  last save. `stored` is the default the page has read, `null` for none, and `undefined` while
 *  the defaults have not been read: the control then takes no pick and does not read
 *  Unassigned, since nobody knows yet whether a default is set. */
function DefaultAssigneeCell({
  logins,
  onReach,
  project,
  readFailed,
  stored,
}: {
  logins: readonly string[];
  onReach: () => void;
  project: string;
  readFailed: boolean;
  stored: string | null | undefined;
}): ReactNode {
  const write = useDefaultAssigneeWrite(project);
  const unread = stored === undefined ? (readFailed ? "Unavailable" : "Loading…") : null;
  return (
    <>
      <DefaultAssigneeSelect
        assignee={write.shown === undefined ? (stored ?? null) : write.shown}
        logins={logins}
        onPick={write.submit}
        onReach={onReach}
        project={project}
        unread={unread}
      />
      {write.error === null ? null : (
        <div className="mt-2">
          <QueryError
            message={apiErrorMessage(write.error, "Couldn't update the default assignee.")}
            onRetry={write.retry}
            retrying={write.pending}
          />
        </div>
      )}
    </>
  );
}

/** One project's default-assignee saves. Each pick is sent, in pick order and one at a time per
 *  project: the mutation scope queues them in the QueryClient's mutation cache, so a save still
 *  out or waiting when the row unmounts keeps its place, and a pick on a remounted row waits
 *  behind it. The control shows the newest pick still pending, from whichever mount made it, so
 *  a remounted row shows the pick in flight; a failed save drops out of that list, so the control
 *  goes back to the stored value or a newer pending pick. Each answer lands in the defaults
 *  cache, so the control does not flash back to the old value, and the defaults are refetched
 *  after each save settles. The server ends at the reader's last pick. */
function useDefaultAssigneeWrite(project: string): {
  /** This row's refused save, until the next attempt starts. */
  error: Error | null;
  pending: boolean;
  /** Resends this row's refused value; a no-op unless its last save failed. */
  retry: () => void;
  /** The newest pick still saving or queued; `undefined` shows the stored value. */
  shown: string | null | undefined;
  submit: (assignee: string | null) => void;
} {
  const queryClient = useQueryClient();
  const defaultsKey = projectDefaultAssigneesQuery().queryKey;
  const mutationKey = ["project-default-assignee", project];
  const mutation = useMutation({
    mutationKey,
    scope: { id: `project-default-assignee:${project}` },
    mutationFn: (assignee: string | null) => api.putProjectDefaultAssignee(project, { assignee }),
    // A defaults read already out was asked before this save and could land over its answer.
    onMutate: () => queryClient.cancelQueries({ queryKey: defaultsKey }),
    onSuccess: (saved) => {
      queryClient.setQueryData<ProjectDefaultAssignee[]>(defaultsKey, (rows) => {
        if (rows === undefined) {
          return rows;
        }
        const others = rows.filter((row) => row.project !== saved.project);
        return saved.assignee === null ? others : [...others, saved];
      });
    },
    // Not awaited: the next pick in the scope need not wait for this refetch.
    onSettled: () => {
      void queryClient.invalidateQueries({ queryKey: defaultsKey });
    },
  });
  // The cache lists mutations in the order they were made, so the last is the newest pick.
  const pendingPicks = useMutationState<
    string | null | undefined,
    Mutation<ProjectDefaultAssignee, Error, string | null>
  >({
    filters: { mutationKey, status: "pending" },
    select: (pending) => pending.state.variables,
  });
  return {
    error: mutation.error,
    pending: mutation.isPending,
    retry: () => {
      if (mutation.isError && mutation.variables !== undefined) {
        mutation.mutate(mutation.variables);
      }
    },
    shown: pendingPicks.at(-1),
    submit: mutation.mutate,
  };
}

/** One project's default-assignee picker: everyone who has signed in, with Unassigned first for
 *  clearing it. `onReach` fires the moment a reader points at, touches or focuses the control,
 *  which is when the people list is fetched. Labelled `Default assignee of <KEY>`. While the
 *  defaults are unread, `unread` is the control's only option and the control is disabled. */
function DefaultAssigneeSelect({
  assignee,
  logins,
  onPick,
  onReach,
  project,
  unread,
}: {
  assignee: string | null;
  logins: readonly string[];
  onPick: (assignee: string | null) => void;
  onReach: () => void;
  project: string;
  unread: string | null;
}): ReactNode {
  // A stored default whose person the list does not carry still holds the project; keep it
  // selectable so the control never shows a value its options lack.
  const options = assignee === null || logins.includes(assignee) ? logins : [assignee, ...logins];
  return (
    <select
      aria-label={`Default assignee of ${project}`}
      className={`rounded-md px-3 py-2 disabled:cursor-not-allowed disabled:opacity-50 ${inputClasses(false)} ${textPrimaryOnSurface}`}
      disabled={unread !== null}
      onChange={(event) => onPick(event.target.value === "" ? null : event.target.value)}
      onFocus={onReach}
      onPointerEnter={onReach}
      onTouchStart={onReach}
      value={unread ?? assignee ?? ""}
    >
      {unread === null ? (
        <>
          <option value="">Unassigned</option>
          {options.map((login) => (
            <option key={login} value={login}>
              {login}
            </option>
          ))}
        </>
      ) : (
        <option value={unread}>{unread}</option>
      )}
    </select>
  );
}
