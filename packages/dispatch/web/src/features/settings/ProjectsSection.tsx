import { PROJECT_NAME_MAX } from "@legion/contracts";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
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
  const setDefaultAssignee = useMutation({
    mutationFn: ({ assignee, project }: ProjectDefaultAssignee) =>
      api.putProjectDefaultAssignee(project, { assignee }),
    onSettled: () =>
      void queryClient.invalidateQueries({ queryKey: projectDefaultAssigneesQuery().queryKey }),
  });
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
                        <DefaultAssigneeSelect
                          assignee={defaultAssignees.get(candidate.key) ?? null}
                          logins={logins}
                          onPick={(assignee) =>
                            setDefaultAssignee.mutate({ assignee, project: candidate.key })
                          }
                          onReach={() => setWantUsers(true)}
                          project={candidate.key}
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
            so a default assignee set on that project reaches that person for every one of its
            issues.
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
          {setDefaultAssignee.isError ? (
            <div className="mt-4">
              <QueryError
                message={apiErrorMessage(
                  setDefaultAssignee.error,
                  "Couldn't update the default assignee."
                )}
                onRetry={() => setDefaultAssignee.mutate(setDefaultAssignee.variables)}
                retrying={setDefaultAssignee.isPending}
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

/** One project's default-assignee picker: everyone who has signed in, with Unassigned first for
 *  clearing it. `onReach` fires the moment a reader points at, touches or focuses the control,
 *  which is when the people list is fetched. Labelled `Default assignee of <KEY>`. */
function DefaultAssigneeSelect({
  assignee,
  logins,
  onPick,
  onReach,
  project,
}: {
  assignee: string | null;
  logins: readonly string[];
  onPick: (assignee: string | null) => void;
  onReach: () => void;
  project: string;
}): ReactNode {
  // A stored default whose person the list does not carry still holds the project; keep it
  // selectable so the control never shows a value its options lack.
  const options = assignee === null || logins.includes(assignee) ? logins : [assignee, ...logins];
  return (
    <select
      aria-label={`Default assignee of ${project}`}
      className={`rounded-md px-3 py-2 ${inputClasses(false)} ${textPrimaryOnSurface}`}
      onChange={(event) => onPick(event.target.value === "" ? null : event.target.value)}
      onFocus={onReach}
      onPointerEnter={onReach}
      onTouchStart={onReach}
      value={assignee ?? ""}
    >
      <option value="">Unassigned</option>
      {options.map((login) => (
        <option key={login} value={login}>
          {login}
        </option>
      ))}
    </select>
  );
}
