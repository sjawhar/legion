import { PROJECT_NAME_MAX } from "@legion/contracts";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { type FormEvent, type ReactNode, useState } from "react";

import { api, apiErrorMessage } from "../../api/client";
import { projectsQuery } from "../../api/queries";
import type { CreateProjectInput } from "../../api/types";
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
import { LandedWithoutAnswer } from "../writes/LandedWithoutAnswer";
import { useKeyedWrite } from "../writes/useKeyedWrite";
import {
  settingsFieldLabel,
  settingsMonoInput,
  settingsSubmitButton,
  settingsTableHead,
  settingsTableRow,
  settingsTableWrapper,
} from "./classes";

/** Lists native projects and lets a human create one; the Settings route's repository
 * mapping form reads the same `["projects"]` query, so a project created here is
 * immediately selectable there without any explicit wiring between the two sections. */
export function ProjectsSection(): ReactNode {
  const queryClient = useQueryClient();
  const [projectKey, setProjectKey] = useState("");
  const [projectName, setProjectName] = useState("");
  /** The project the last create made with no answer to show it by, until the next edit. */
  const [madeUnanswered, setMadeUnanswered] = useState<string | null>(null);
  const projectSubmitGuard = useSubmitGuard();
  const projects = useQuery(projectsQuery());
  // The form shows a refused create beside the key it was refused over.
  const createProject = useKeyedWrite({
    inline: true,
    label: (input: CreateProjectInput) => `New project ${input.key}`,
    write: (input, bounds) => api.createProject(input, bounds),
    onSettled: () => projectSubmitGuard.release(),
    onLanded: (project, input) => {
      void queryClient.invalidateQueries({ queryKey: projectsQuery().queryKey });
      setProjectKey("");
      setProjectName("");
      setMadeUnanswered(project === null ? input.key : null);
    },
  });
  const submitProject = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    // An emptied form - a create that landed - sends nothing, whatever submits it.
    if (projectKey === "" || projectName.trim() === "") return;
    projectSubmitGuard.guard(() => createProject.mutate({ key: projectKey, name: projectName }));
  };

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
                    Created
                  </th>
                </tr>
              </thead>
              <tbody>
                {projects.data.length === 0 ? (
                  <tr>
                    <td className={`px-4 py-5 ${textMutedOnSurface}`} colSpan={3}>
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
                      <td className={`px-4 py-3 ${textMutedOnSurface}`}>
                        <Timestamp at={candidate.created_at} />
                      </td>
                    </tr>
                  ))
                )}
              </tbody>
            </table>
          </div>

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
                onChange={(event) => {
                  setMadeUnanswered(null);
                  setProjectKey(event.target.value.trim().toUpperCase());
                }}
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
          {madeUnanswered === null ? null : (
            <div className="mt-4">
              <LandedWithoutAnswer what={`Project ${madeUnanswered}`} />
            </div>
          )}
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
