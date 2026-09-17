import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { type FormEvent, type ReactNode, useState } from "react";

import { ApiError, api } from "../../api/client";
import { QueryError } from "../../components/QueryError";
import { useSubmitGuard } from "../../hooks/useSubmitGuard";
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

/** One architecture source per project: the repository and branch its architecture
 * documents are imported from. Saving runs the server's GitHub App access check, so a
 * listed row has proven access — it shows "Access verified" until a sync reports
 * otherwise — and a failed check surfaces the server's reason inline. */
export function ArchitectureSourcesSection(): ReactNode {
  const queryClient = useQueryClient();
  const [project, setProject] = useState("");
  const [repository, setRepository] = useState("");
  const [branch, setBranch] = useState("main");
  const submitGuard = useSubmitGuard();
  const sources = useQuery({
    queryKey: ["architecture-sources"],
    queryFn: () => api.listArchitectureSources(),
  });
  const projects = useQuery({ queryKey: ["projects"], queryFn: () => api.listProjects() });
  const saveSource = useMutation({
    mutationFn: (input: { project: string; repo: string; branch: string }) =>
      api.putArchitectureSource(input.project, { branch: input.branch, repo: input.repo }),
    onSettled: () => submitGuard.release(),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["architecture-sources"] });
      setProject("");
      setRepository("");
      setBranch("main");
    },
  });
  const deleteSource = useMutation({
    mutationFn: (key: string) => api.deleteArchitectureSource(key),
    onSuccess: () => void queryClient.invalidateQueries({ queryKey: ["architecture-sources"] }),
  });
  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    submitGuard.guard(() => saveSource.mutate({ branch, project, repo: repository }));
  };

  return (
    <section aria-labelledby="architecture-sources-heading" className="mt-10">
      <h2
        className={`text-xl font-semibold ${textPrimaryOnCanvas}`}
        id="architecture-sources-heading"
      >
        Architecture sources
      </h2>
      <p className={`mt-1 text-sm ${textSecondaryOnCanvas}`}>
        Each project can import its architecture documents from one repository branch. Saving
        verifies the Dispatch GitHub App can read it.
      </p>

      {sources.isPending || projects.isPending ? (
        <p className={`mt-6 ${textMutedOnCanvas}`}>Loading architecture sources…</p>
      ) : null}
      {sources.isError ? (
        <div className="mt-6">
          <QueryError
            message="Couldn't load architecture sources."
            onRetry={() => void sources.refetch()}
            retrying={sources.isFetching}
          />
        </div>
      ) : null}
      {sources.isSuccess && projects.isSuccess ? (
        <>
          <div className={`mt-6 overflow-x-auto rounded-xl border ${card}`}>
            <table className="w-full text-left text-sm">
              <thead
                className={`border-b ${surfaceMutedBg} ${borderDefault} ${textSecondaryOnSurface}`}
              >
                <tr>
                  <th className="px-4 py-3 font-semibold" scope="col">
                    Project
                  </th>
                  <th className="px-4 py-3 font-semibold" scope="col">
                    Repository
                  </th>
                  <th className="px-4 py-3 font-semibold" scope="col">
                    Branch
                  </th>
                  <th className="px-4 py-3 font-semibold" scope="col">
                    Status
                  </th>
                  <th className="px-4 py-3" scope="col">
                    <span className="sr-only">Actions</span>
                  </th>
                </tr>
              </thead>
              <tbody>
                {sources.data.length === 0 ? (
                  <tr>
                    <td className={`px-4 py-5 ${textMutedOnSurface}`} colSpan={5}>
                      No architecture sources yet.
                    </td>
                  </tr>
                ) : (
                  sources.data.map((source) => (
                    <tr className={`border-b last:border-0 ${borderDefault}`} key={source.project}>
                      <td className={`px-4 py-3 font-mono ${textPrimaryOnSurface}`}>
                        {source.project}
                      </td>
                      <td className={`px-4 py-3 font-mono ${textPrimaryOnSurface}`}>
                        {source.repo}
                      </td>
                      <td className={`px-4 py-3 font-mono ${textSecondaryOnSurface}`}>
                        {source.branch}
                      </td>
                      <td className={`px-4 py-3 ${textSecondaryOnSurface}`}>
                        {source.last_error === null ? (
                          "Access verified"
                        ) : (
                          <span className={dangerText}>{source.last_error}</span>
                        )}
                      </td>
                      <td className="px-4 py-3 text-right">
                        <button
                          aria-label={`Delete architecture source for ${source.project}`}
                          className={`font-medium disabled:cursor-not-allowed disabled:opacity-50 ${dangerText} ${dangerHoverText}`}
                          disabled={deleteSource.isPending || saveSource.isPending}
                          onClick={() => deleteSource.mutate(source.project)}
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
            className={`mt-6 grid gap-4 rounded-xl border p-4 sm:grid-cols-2 sm:items-end ${borderDefault}`}
            onSubmit={submit}
          >
            <label
              className={`grid gap-1 text-sm font-medium ${textSecondaryOnCanvas}`}
              htmlFor="architecture-source-project"
            >
              Source project
              <select
                className={`rounded-md px-3 py-2 ${inputClasses(false)} ${textPrimaryOnSurface}`}
                disabled={saveSource.isPending}
                id="architecture-source-project"
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
            <label
              className={`grid gap-1 text-sm font-medium ${textSecondaryOnCanvas}`}
              htmlFor="architecture-source-repository"
            >
              Source repository
              <input
                className={`rounded-md px-3 py-2 font-mono ${inputClasses(false)} ${textPrimaryOnSurface}`}
                disabled={saveSource.isPending}
                id="architecture-source-repository"
                onChange={(event) => setRepository(event.target.value)}
                pattern="[^\/\s]+\/[^\/\s]+"
                placeholder="owner/repo"
                required
                value={repository}
              />
            </label>
            <label
              className={`grid gap-1 text-sm font-medium ${textSecondaryOnCanvas}`}
              htmlFor="architecture-source-branch"
            >
              Branch
              <input
                className={`rounded-md px-3 py-2 font-mono ${inputClasses(false)} ${textPrimaryOnSurface}`}
                disabled={saveSource.isPending}
                id="architecture-source-branch"
                onChange={(event) => setBranch(event.target.value)}
                required
                value={branch}
              />
            </label>
            <button
              className={`rounded-md px-4 py-2 font-medium disabled:cursor-not-allowed disabled:opacity-50 ${primaryButtonBg} ${primaryButtonHoverBg}`}
              disabled={saveSource.isPending || deleteSource.isPending}
              type="submit"
            >
              Add source
            </button>
          </form>
          {saveSource.isError ? (
            <div className="mt-4">
              <QueryError
                message={
                  saveSource.error instanceof ApiError
                    ? saveSource.error.message
                    : "Couldn't save the architecture source."
                }
                onRetry={() => submitGuard.retryLast(saveSource)}
                retrying={saveSource.isPending}
              />
            </div>
          ) : null}
          {deleteSource.isError ? (
            <div className="mt-4">
              <QueryError
                message="Couldn't delete the architecture source."
                onRetry={() => deleteSource.mutate(deleteSource.variables)}
                retrying={deleteSource.isPending}
              />
            </div>
          ) : null}
        </>
      ) : null}
    </section>
  );
}
