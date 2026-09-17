import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { type FormEvent, type ReactNode, useState } from "react";

import { api, apiErrorMessage } from "../../api/client";
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
  settingsFieldLabel,
  settingsMonoInput,
  settingsSubmitButton,
  settingsTableHead,
  settingsTableRow,
  settingsTableWrapper,
} from "./classes";
import { ProjectSelect } from "./ProjectSelect";

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
  // A refresh settles into the row either way: a rejected model comes back 200
  // with the reason in last_error, and even a 409 (access revoked, branch gone)
  // recorded its reason on the row before failing, so both invalidate.
  const refreshSource = useMutation({
    mutationFn: (key: string) => api.syncArchitectureSource(key),
    onSettled: () => void queryClient.invalidateQueries({ queryKey: ["architecture-sources"] }),
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
                    <tr className={settingsTableRow} key={source.project}>
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
                              </div>
                            ) : null}
                          </>
                        ) : source.last_commit !== null ? (
                          <>
                            Synced{" "}
                            <span className="font-mono">{source.last_commit.slice(0, 12)}</span>
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
                          aria-label={`Refresh architecture source for ${source.project}`}
                          className={`mr-4 font-medium disabled:cursor-not-allowed disabled:opacity-50 ${linkText} ${linkHoverText}`}
                          disabled={
                            refreshSource.isPending ||
                            deleteSource.isPending ||
                            saveSource.isPending
                          }
                          onClick={() => refreshSource.mutate(source.project)}
                          type="button"
                        >
                          {refreshSource.isPending && refreshSource.variables === source.project
                            ? "Refreshing…"
                            : "Refresh"}
                        </button>
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
                pattern="[^\/\s]+\/[^\/\s]+"
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
