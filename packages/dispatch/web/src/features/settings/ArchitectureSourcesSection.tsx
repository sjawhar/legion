import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { type FormEvent, type ReactNode, useState } from "react";

import { api, apiErrorMessage } from "../../api/client";
import { architectureSourcesQuery, projectsQuery } from "../../api/queries";
import { QueryError } from "../../components/QueryError";
import { useSubmitGuard } from "../../hooks/useSubmitGuard";
import {
  borderDefault,
  dangerHoverText,
  dangerText,
  linkHoverText,
  linkText,
  textMutedOnCanvas,
  textMutedOnSurface,
  textPrimaryOnCanvas,
  textPrimaryOnSurface,
  textSecondaryOnCanvas,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { Timestamp } from "../refs/Timestamp";
import {
  ownerRepoPattern,
  settingsFieldLabel,
  settingsMonoInput,
  settingsSubmitButton,
  settingsTableHead,
  settingsTableRow,
  settingsTableWrapper,
} from "./classes";
import { ProjectSelect } from "./ProjectSelect";

/** A source's address: a project and one of its repositories. */
interface SourceKey {
  readonly project: string;
  readonly repo: string;
}

/** A project's architecture sources, one row per repository: the branch its architecture
 * documents are imported from, the commit imported and how many components it supplies. Saving
 * runs the server's GitHub App access check, so a listed row has proven access — it shows
 * "Access verified" until a sync reports otherwise — and a failed check surfaces the server's
 * reason inline. Saving a repository the project already has changes that source's branch. */
export function ArchitectureSourcesSection(): ReactNode {
  const queryClient = useQueryClient();
  const [project, setProject] = useState("");
  const [repository, setRepository] = useState("");
  const [branch, setBranch] = useState("main");
  const submitGuard = useSubmitGuard();
  const sources = useQuery(architectureSourcesQuery());
  const projects = useQuery(projectsQuery());
  const saveSource = useMutation({
    mutationFn: (input: { project: string; repo: string; branch: string }) =>
      api.putArchitectureSource(input.project, input.repo, { branch: input.branch }),
    onSettled: () => submitGuard.release(),
    onSuccess: () => {
      // The settings list's key prefixes every project's own list, so both refresh.
      void queryClient.invalidateQueries({ queryKey: architectureSourcesQuery().queryKey });
      setProject("");
      setRepository("");
      setBranch("main");
    },
  });
  const deleteSource = useMutation({
    mutationFn: (source: SourceKey) => api.deleteArchitectureSource(source.project, source.repo),
    onSuccess: () =>
      void queryClient.invalidateQueries({ queryKey: architectureSourcesQuery().queryKey }),
  });
  // A refresh settles into the row either way: a rejected model comes back 200
  // with the reason in last_error, and even a 409 (access revoked, branch gone)
  // recorded its reason on the row before failing, so both invalidate.
  const refreshSource = useMutation({
    mutationFn: (source: SourceKey) => api.syncArchitectureSource(source.project, source.repo),
    onSettled: () =>
      void queryClient.invalidateQueries({ queryKey: architectureSourcesQuery().queryKey }),
  });
  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    submitGuard.guard(() => saveSource.mutate({ branch, project, repo: repository }));
  };

  return (
    <section aria-labelledby="architecture-sources-heading">
      <h2
        className={`text-xl font-semibold ${textPrimaryOnCanvas}`}
        id="architecture-sources-heading"
      >
        Architecture sources
      </h2>
      <p className={`mt-1 text-sm ${textSecondaryOnCanvas}`}>
        A project can import its architecture documents from several repositories, each on one
        branch. Its component model is every source's components together, so a component id must
        not repeat across a project's sources. Saving verifies the Dispatch GitHub App can read the
        repository and that the branch exists in it (a branch name, not a commit or tag); saving a
        repository the project already has changes its branch.
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
          <div className={settingsTableWrapper}>
            <table className="w-full text-left text-sm">
              <thead className={settingsTableHead}>
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
                    <tr className={settingsTableRow} key={`${source.project} ${source.repo}`}>
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
                        {source.last_error !== null ? (
                          <>
                            <span className={dangerText}>{source.last_error}</span>
                            {source.last_commit !== null ? (
                              <div className={textMutedOnSurface}>
                                Previous model stays up at{" "}
                                <span className="font-mono">{source.last_commit.slice(0, 12)}</span>
                                {` · ${componentCount(source.components)}`}
                              </div>
                            ) : null}
                          </>
                        ) : source.last_commit !== null ? (
                          <>
                            Synced{" "}
                            <span className="font-mono">{source.last_commit.slice(0, 12)}</span>
                            {` · ${componentCount(source.components)}`}
                            {source.last_sync_at !== null ? (
                              <>
                                {" "}
                                <Timestamp
                                  at={source.last_sync_at}
                                  className={textMutedOnSurface}
                                />
                              </>
                            ) : null}
                          </>
                        ) : (
                          "Access verified"
                        )}
                      </td>
                      <td className="px-4 py-3 text-right">
                        <button
                          aria-label={`Refresh architecture source ${source.repo} for ${source.project}`}
                          className={`mr-4 font-medium disabled:cursor-not-allowed disabled:opacity-50 ${linkText} ${linkHoverText}`}
                          disabled={
                            refreshSource.isPending ||
                            deleteSource.isPending ||
                            saveSource.isPending
                          }
                          onClick={() =>
                            refreshSource.mutate({ project: source.project, repo: source.repo })
                          }
                          type="button"
                        >
                          {refreshSource.isPending &&
                          refreshSource.variables.project === source.project &&
                          refreshSource.variables.repo === source.repo
                            ? "Refreshing…"
                            : "Refresh"}
                        </button>
                        <button
                          aria-label={`Delete architecture source ${source.repo} for ${source.project}`}
                          className={`font-medium disabled:cursor-not-allowed disabled:opacity-50 ${dangerText} ${dangerHoverText}`}
                          disabled={deleteSource.isPending || saveSource.isPending}
                          onClick={() =>
                            deleteSource.mutate({ project: source.project, repo: source.repo })
                          }
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
            <label className={settingsFieldLabel} htmlFor="architecture-source-project">
              Source project
              <ProjectSelect
                disabled={saveSource.isPending}
                id="architecture-source-project"
                onChange={setProject}
                projects={projects.data}
                value={project}
              />
            </label>
            <label className={settingsFieldLabel} htmlFor="architecture-source-repository">
              Source repository
              <input
                className={settingsMonoInput}
                disabled={saveSource.isPending}
                id="architecture-source-repository"
                onChange={(event) => setRepository(event.target.value)}
                pattern={ownerRepoPattern}
                placeholder="owner/repo"
                required
                value={repository}
              />
            </label>
            <label className={settingsFieldLabel} htmlFor="architecture-source-branch">
              Branch
              <input
                className={settingsMonoInput}
                disabled={saveSource.isPending}
                id="architecture-source-branch"
                onChange={(event) => setBranch(event.target.value)}
                required
                value={branch}
              />
            </label>
            <button
              className={settingsSubmitButton}
              disabled={saveSource.isPending || deleteSource.isPending}
              type="submit"
            >
              Add source
            </button>
          </form>
          {saveSource.isError ? (
            <div className="mt-4">
              <QueryError
                message={apiErrorMessage(
                  saveSource.error,
                  "Couldn't save the architecture source."
                )}
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
          {refreshSource.isError ? (
            <div className="mt-4">
              <QueryError
                message={apiErrorMessage(
                  refreshSource.error,
                  "Couldn't refresh the architecture source."
                )}
                onRetry={() => refreshSource.mutate(refreshSource.variables)}
                retrying={refreshSource.isPending}
              />
            </div>
          ) : null}
        </>
      ) : null}
    </section>
  );
}

/** `1 component`, `N components`: how much of the project's model one source supplies. */
function componentCount(count: number): string {
  return count === 1 ? "1 component" : `${count} components`;
}
