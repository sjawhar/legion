import { canonicalRepo } from "@legion/contracts/repo";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { type FormEvent, type ReactNode, useState } from "react";
import { api } from "../../api/client";
import type { RepoProject } from "../../api/types";
import { QueryError } from "../../components/QueryError";
import {
  borderDefault,
  card,
  dangerHoverText,
  dangerText,
  inputClasses,
  primaryButtonBg,
  primaryButtonHoverBg,
  surfaceMutedBg,
  textMutedOnCanvas,
  textMutedOnSurface,
  textPrimaryOnCanvas,
  textPrimaryOnSurface,
  textSecondaryOnCanvas,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { useDocumentTitle } from "../shell/useDocumentTitle";
import { ProjectsSection } from "./ProjectsSection";

type FailedAction =
  | { readonly kind: "put"; readonly project: string; readonly repo: string }
  | { readonly kind: "delete"; readonly repo: string };

export function SettingsPage(): ReactNode {
  useDocumentTitle("Settings · Dispatch");
  const queryClient = useQueryClient();
  const [repository, setRepository] = useState("");
  const [project, setProject] = useState("");
  const [failedAction, setFailedAction] = useState<FailedAction>();
  const mappings = useQuery({ queryKey: ["repo-projects"], queryFn: () => api.listRepoProjects() });
  const projects = useQuery({ queryKey: ["projects"], queryFn: () => api.listProjects() });
  const putMapping = useMutation<
    RepoProject,
    Error,
    { repo: string; project: string },
    { previous: RepoProject[] | undefined }
  >({
    mutationFn: ({ repo, project }: { repo: string; project: string }) =>
      api.putRepoProject(repo, { project }),
    onError: (_error, variables, context) => {
      queryClient.setQueryData(["repo-projects"], context?.previous);
      setFailedAction({ kind: "put", ...variables });
    },
    onMutate: async (variables) => {
      await queryClient.cancelQueries({ queryKey: ["repo-projects"] });
      const previous = queryClient.getQueryData<RepoProject[]>(["repo-projects"]);
      queryClient.setQueryData<RepoProject[]>(["repo-projects"], (current = []) => [
        ...current.filter((mapping) => mapping.repo !== variables.repo),
        {
          created_at: new Date().toISOString(),
          created_by: { id: "you", kind: "user" },
          project: variables.project,
          repo: variables.repo,
        },
      ]);
      return { previous };
    },
    onSuccess: (mapping) => {
      queryClient.setQueryData<RepoProject[]>(["repo-projects"], (current = []) => [
        ...current.filter((candidate) => candidate.repo !== mapping.repo),
        mapping,
      ]);
      setFailedAction(undefined);
      setRepository("");
      setProject("");
    },
  });
  const deleteMapping = useMutation<void, Error, string, { previous: RepoProject[] | undefined }>({
    mutationFn: (repo: string) => api.deleteRepoProject(repo),
    onError: (_error, repo, context) => {
      queryClient.setQueryData(["repo-projects"], context?.previous);
      setFailedAction({ kind: "delete", repo });
    },
    onMutate: async (repo) => {
      await queryClient.cancelQueries({ queryKey: ["repo-projects"] });
      const previous = queryClient.getQueryData<RepoProject[]>(["repo-projects"]);
      queryClient.setQueryData<RepoProject[]>(["repo-projects"], (current = []) =>
        current.filter((mapping) => mapping.repo !== repo)
      );
      return { previous };
    },
    onSuccess: () => setFailedAction(undefined),
  });

  const retry = () => {
    if (failedAction?.kind === "put") {
      putMapping.mutate({ project: failedAction.project, repo: failedAction.repo });
      return;
    }
    if (failedAction?.kind === "delete") {
      deleteMapping.mutate(failedAction.repo);
    }
  };
  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const [owner, repo] = repository.split("/", 2);
    putMapping.mutate({ project, repo: canonicalRepo(owner ?? "", repo ?? "") });
  };

  return (
    <section className="max-w-4xl">
      <header className="mb-6">
        <h1 className={`text-2xl font-semibold ${textPrimaryOnCanvas}`}>Settings</h1>
        <p className={`mt-1 ${textSecondaryOnCanvas}`}>
          Control where external repository issues appear in Dispatch.
        </p>
      </header>
      <ProjectsSection />
      <section aria-labelledby="repository-projects-heading">
        <h2
          className={`text-xl font-semibold ${textPrimaryOnCanvas}`}
          id="repository-projects-heading"
        >
          Repositories → Projects
        </h2>
        <p className={`mt-1 text-sm ${textSecondaryOnCanvas}`}>
          External issues use these mappings before falling back to the default project.
        </p>

        {mappings.isPending || projects.isPending ? (
          <p className={`mt-6 ${textMutedOnCanvas}`}>Loading settings…</p>
        ) : null}
        {mappings.isError ? (
          <div className="mt-6">
            <QueryError
              message="Couldn't load repository mappings."
              onRetry={() => void mappings.refetch()}
              retrying={mappings.isFetching}
            />
          </div>
        ) : null}
        {projects.isError ? (
          <div className="mt-6">
            <QueryError
              message="Couldn't load projects."
              onRetry={() => void projects.refetch()}
              retrying={projects.isFetching}
            />
          </div>
        ) : null}
        {mappings.isSuccess && projects.isSuccess ? (
          <>
            <div className={`mt-6 overflow-x-auto rounded-xl border ${card}`}>
              <table className="w-full text-left text-sm">
                <thead
                  className={`border-b ${surfaceMutedBg} ${borderDefault} ${textSecondaryOnSurface}`}
                >
                  <tr>
                    <th className="px-4 py-3 font-semibold" scope="col">
                      Repository
                    </th>
                    <th className="px-4 py-3 font-semibold" scope="col">
                      Project
                    </th>
                    <th className="px-4 py-3" scope="col">
                      <span className="sr-only">Actions</span>
                    </th>
                  </tr>
                </thead>
                <tbody>
                  {mappings.data.length === 0 ? (
                    <tr>
                      <td className={`px-4 py-5 ${textMutedOnSurface}`} colSpan={3}>
                        No repository mappings yet.
                      </td>
                    </tr>
                  ) : (
                    mappings.data.map((mapping) => (
                      <tr className={`border-b last:border-0 ${borderDefault}`} key={mapping.repo}>
                        <td className={`px-4 py-3 font-mono ${textPrimaryOnSurface}`}>
                          {mapping.repo}
                        </td>
                        <td className={`px-4 py-3 ${textSecondaryOnSurface}`}>{mapping.project}</td>
                        <td className="px-4 py-3 text-right">
                          <button
                            aria-label={`Delete mapping for ${mapping.repo}`}
                            className={`font-medium disabled:cursor-not-allowed disabled:opacity-50 ${dangerText} ${dangerHoverText}`}
                            disabled={deleteMapping.isPending || putMapping.isPending}
                            onClick={() => deleteMapping.mutate(mapping.repo)}
                            type="button"
                          >
                            Delete
                          </button>
                        </td>
                      </tr>
                    ))
                  )}
                </tbody>
              </table>
            </div>

            <form
              className={`mt-6 grid gap-4 rounded-xl border p-4 sm:grid-cols-[1fr_1fr_auto] sm:items-end ${borderDefault}`}
              onSubmit={submit}
            >
              <label
                className={`grid gap-1 text-sm font-medium ${textSecondaryOnCanvas}`}
                htmlFor="repository"
              >
                Repository
                <input
                  className={`rounded-md px-3 py-2 font-mono ${inputClasses(false)} ${textPrimaryOnSurface}`}
                  id="repository"
                  onChange={(event) => setRepository(event.target.value)}
                  pattern="[^/\s]+/[^/\s]+"
                  placeholder="owner/repo"
                  required
                  value={repository}
                />
              </label>
              <label
                className={`grid gap-1 text-sm font-medium ${textSecondaryOnCanvas}`}
                htmlFor="project"
              >
                Project
                <select
                  className={`rounded-md px-3 py-2 ${inputClasses(false)} ${textPrimaryOnSurface}`}
                  id="project"
                  onChange={(event) => setProject(event.target.value)}
                  required
                  value={project}
                >
                  <option disabled value="">
                    Choose a project
                  </option>
                  {projects.data.map((candidate) => (
                    <option key={candidate.key} value={candidate.key}>
                      {candidate.key} · {candidate.name}
                    </option>
                  ))}
                </select>
              </label>
              <button
                className={`rounded-md px-4 py-2 font-medium disabled:cursor-not-allowed disabled:opacity-50 ${primaryButtonBg} ${primaryButtonHoverBg}`}
                disabled={putMapping.isPending || deleteMapping.isPending}
                type="submit"
              >
                Add mapping
              </button>
            </form>
            {failedAction !== undefined ? (
              <div className="mt-4">
                <QueryError
                  message={
                    failedAction.kind === "put"
                      ? `Couldn't save ${failedAction.repo}.`
                      : `Couldn't delete ${failedAction.repo}.`
                  }
                  onRetry={retry}
                  retrying={putMapping.isPending || deleteMapping.isPending}
                />
              </div>
            ) : null}
          </>
        ) : null}
      </section>
    </section>
  );
}
