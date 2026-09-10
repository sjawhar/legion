import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { type FormEvent, type ReactNode, useState } from "react";

import { ApiError, api } from "../../api/client";
import type { CreateProjectInput } from "../../api/types";
import { QueryError } from "../../components/QueryError";
import { useSubmitGuard } from "../../hooks/useSubmitGuard";
import { Timestamp } from "../refs/Timestamp";

/** Lists native projects and lets a human create one; the Settings route's repository
 * mapping form reads the same `["projects"]` query, so a project created here is
 * immediately selectable there without any explicit wiring between the two sections. */
export function ProjectsSection(): ReactNode {
  const queryClient = useQueryClient();
  const [projectKey, setProjectKey] = useState("");
  const [projectName, setProjectName] = useState("");
  const projectSubmitGuard = useSubmitGuard();
  const projects = useQuery({ queryKey: ["projects"], queryFn: () => api.listProjects() });
  const createProject = useMutation({
    mutationFn: (input: CreateProjectInput) => api.createProject(input),
    onSettled: () => projectSubmitGuard.release(),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["projects"] });
      setProjectKey("");
      setProjectName("");
    },
  });
  const submitProject = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    projectSubmitGuard.guard(() => createProject.mutate({ key: projectKey, name: projectName }));
  };

  return (
    <section aria-labelledby="projects-heading" className="mb-10">
      <h2 className="text-xl font-semibold" id="projects-heading">
        Projects
      </h2>
      <p className="mt-1 text-sm text-slate-600">
        Native issues are grouped under a project key, such as CORE-1.
      </p>

      {projects.isPending ? <p className="mt-6 text-slate-500">Loading projects…</p> : null}
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
          <div className="mt-6 overflow-x-auto rounded-xl border border-slate-200 bg-white">
            <table className="w-full text-left text-sm">
              <thead className="border-b border-slate-200 bg-slate-50 text-slate-600">
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
                    <td className="px-4 py-5 text-slate-500" colSpan={3}>
                      No projects yet.
                    </td>
                  </tr>
                ) : (
                  projects.data.map((candidate) => (
                    <tr className="border-b border-slate-100 last:border-0" key={candidate.key}>
                      <td className="px-4 py-3 font-mono text-slate-800">{candidate.key}</td>
                      <td className="px-4 py-3 text-slate-700">{candidate.name}</td>
                      <td className="px-4 py-3 text-slate-500">
                        <Timestamp at={candidate.created_at} />
                      </td>
                    </tr>
                  ))
                )}
              </tbody>
            </table>
          </div>

          <form
            className="mt-6 grid gap-4 rounded-xl border border-slate-200 p-4 sm:grid-cols-[1fr_2fr_auto] sm:items-end"
            onSubmit={submitProject}
          >
            <label className="grid gap-1 text-sm font-medium text-slate-700" htmlFor="project-key">
              Key
              <input
                className="rounded-md border border-slate-300 px-3 py-2 font-mono text-slate-900"
                disabled={createProject.isPending}
                id="project-key"
                onChange={(event) => setProjectKey(event.target.value.trim().toUpperCase())}
                placeholder="CORE"
                required
                value={projectKey}
              />
            </label>
            <label className="grid gap-1 text-sm font-medium text-slate-700" htmlFor="project-name">
              Name
              <input
                className="rounded-md border border-slate-300 px-3 py-2 text-slate-900"
                disabled={createProject.isPending}
                id="project-name"
                onChange={(event) => setProjectName(event.target.value)}
                placeholder="Core"
                required
                value={projectName}
              />
            </label>
            <button
              className="rounded-md bg-sky-700 px-4 py-2 font-medium text-white hover:bg-sky-800 disabled:cursor-not-allowed disabled:opacity-50"
              disabled={createProject.isPending}
              type="submit"
            >
              New project
            </button>
          </form>
          {createProject.isError ? (
            <div className="mt-4">
              <QueryError
                message={
                  createProject.error instanceof ApiError
                    ? createProject.error.message
                    : "Couldn't create the project."
                }
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
