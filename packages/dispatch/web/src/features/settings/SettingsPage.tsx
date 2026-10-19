import { canonicalRepo } from "@legion/contracts/repo";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { type FormEvent, type ReactNode, useState } from "react";

import { api, apiErrorMessage } from "../../api/client";
import { projectsQuery } from "../../api/queries";
import type { RepoProject } from "../../api/types";
import { QueryError } from "../../components/QueryError";
import {
  borderDefault,
  dangerHoverText,
  dangerText,
  textMutedOnCanvas,
  textMutedOnSurface,
  textPrimaryOnCanvas,
  textPrimaryOnSurface,
  textSecondaryOnCanvas,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { GrantsSection } from "../credentials/GrantsSection";
import { credentialPendingQuery } from "../credentials/pending";
import { DeliverySettingsForm } from "../delivery/DeliverySettingsForm";
import { useDocumentTitle } from "../shell/useDocumentTitle";
import { AgentTokensSection } from "./AgentTokensSection";
import { ArchitectureSourcesSection } from "./ArchitectureSourcesSection";
import {
  ownerRepoPattern,
  settingsFieldLabel,
  settingsMonoInput,
  settingsSectionGap,
  settingsSubmitButton,
  settingsTableHead,
  settingsTableRow,
  settingsTableWrapper,
} from "./classes";
import { ProjectSelect } from "./ProjectSelect";
import { ProjectsSection } from "./ProjectsSection";

interface MappingInput {
  readonly repo: string;
  readonly project: string;
  readonly default?: boolean;
}

/** A mapping write the server refused or never answered, with what the page says about it: the
 *  server's reason when it gave one (a mapping issues still name, say), else a generic line. */
type FailedAction =
  | ({ readonly kind: "put"; readonly message: string } & MappingInput)
  | { readonly kind: "delete"; readonly message: string; readonly repo: string };

/** The mappings with `mapping` in place of its row, and, when it is its project's default, every
 *  other mapping of that project no longer the default: a project has at most one. */
function withMapping(current: RepoProject[], mapping: RepoProject): RepoProject[] {
  return [
    ...current
      .filter((candidate) => candidate.repo !== mapping.repo)
      .map((candidate) =>
        mapping.default && candidate.project === mapping.project && candidate.default
          ? { ...candidate, default: false }
          : candidate
      ),
    mapping,
  ];
}

export function SettingsPage(): ReactNode {
  useDocumentTitle("Settings · Dispatch");
  const queryClient = useQueryClient();
  const [repository, setRepository] = useState("");
  const [project, setProject] = useState("");
  const [failedAction, setFailedAction] = useState<FailedAction>();
  const mappings = useQuery({ queryKey: ["repo-projects"], queryFn: () => api.listRepoProjects() });
  const projects = useQuery(projectsQuery());
  // `null` is a Dispatch with no secrets broker, where Live grants has nothing to list.
  const credentialsAvailable = useQuery(credentialPendingQuery());
  const putMapping = useMutation<
    RepoProject,
    Error,
    MappingInput,
    { previous: RepoProject[] | undefined }
  >({
    mutationFn: ({ repo, ...input }) => api.putRepoProject(repo, input),
    onError: (error, variables, context) => {
      queryClient.setQueryData(["repo-projects"], context?.previous);
      setFailedAction({
        kind: "put",
        message: apiErrorMessage(error, `Couldn't save ${variables.repo}.`),
        ...variables,
      });
    },
    onMutate: async (variables) => {
      await queryClient.cancelQueries({ queryKey: ["repo-projects"] });
      const previous = queryClient.getQueryData<RepoProject[]>(["repo-projects"]);
      const existing = previous?.find((mapping) => mapping.repo === variables.repo);
      queryClient.setQueryData<RepoProject[]>(["repo-projects"], (current = []) =>
        withMapping(current, {
          created_at: new Date().toISOString(),
          created_by: { id: "you", kind: "user" },
          // As the server decides it: the body's default, else the mapping's own while its
          // project is unchanged, else not the default.
          default:
            variables.default ??
            (existing?.project === variables.project ? existing.default : false),
          project: variables.project,
          repo: variables.repo,
        })
      );
      return { previous };
    },
    onSuccess: (mapping, variables) => {
      queryClient.setQueryData<RepoProject[]>(["repo-projects"], (current = []) =>
        withMapping(current, mapping)
      );
      setFailedAction(undefined);
      if (variables.default === undefined) {
        setRepository("");
        setProject("");
      }
    },
    // A write's answer is one row as the server saved it, not the project's mappings when the
    // answer arrives: another write (another browser's default pick, read through a mapping event
    // while this one was on the way) may have changed them since. Reading them again once the
    // write settles leaves the page on the server's state, as useIssueFieldWrite does an issue's.
    onSettled: () => queryClient.invalidateQueries({ queryKey: ["repo-projects"] }),
  });
  const deleteMapping = useMutation<void, Error, string, { previous: RepoProject[] | undefined }>({
    mutationFn: (repo: string) => api.deleteRepoProject(repo),
    onError: (error, repo, context) => {
      queryClient.setQueryData(["repo-projects"], context?.previous);
      setFailedAction({
        kind: "delete",
        message: apiErrorMessage(error, `Couldn't delete ${repo}.`),
        repo,
      });
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
    onSettled: () => queryClient.invalidateQueries({ queryKey: ["repo-projects"] }),
  });

  const retry = () => {
    if (failedAction?.kind === "put") {
      const { kind: _kind, message: _message, ...variables } = failedAction;
      putMapping.mutate(variables);
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
      <div className={settingsSectionGap}>
        <ProjectsSection />
        <AgentTokensSection />
        {credentialsAvailable.isPending || credentialsAvailable.data === null ? null : (
          <GrantsSection />
        )}
        <section aria-labelledby="repository-projects-heading">
          <h2
            className={`text-xl font-semibold ${textPrimaryOnCanvas}`}
            id="repository-projects-heading"
          >
            Repositories → Projects
          </h2>
          <p className={`mt-1 text-sm ${textSecondaryOnCanvas}`}>
            An issue's repository is one of its project's mappings. An issue with none of its own or
            on its parent chain takes its project's Default. External issues use these mappings
            before falling back to the default project.
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
              <div className={settingsTableWrapper}>
                <table className="w-full text-left text-sm">
                  <thead className={settingsTableHead}>
                    <tr>
                      <th className="px-4 py-3 font-semibold" scope="col">
                        Repository
                      </th>
                      <th className="px-4 py-3 font-semibold" scope="col">
                        Project
                      </th>
                      <th className="px-4 py-3 font-semibold" scope="col">
                        Default
                      </th>
                      <th className="px-4 py-3" scope="col">
                        <span className="sr-only">Actions</span>
                      </th>
                    </tr>
                  </thead>
                  <tbody>
                    {mappings.data.length === 0 ? (
                      <tr>
                        <td className={`px-4 py-5 ${textMutedOnSurface}`} colSpan={4}>
                          No repository mappings yet.
                        </td>
                      </tr>
                    ) : (
                      mappings.data.map((mapping) => (
                        <tr className={settingsTableRow} key={mapping.repo}>
                          <td className={`px-4 py-3 font-mono ${textPrimaryOnSurface}`}>
                            {mapping.repo}
                          </td>
                          <td className={`px-4 py-3 ${textSecondaryOnSurface}`}>
                            {mapping.project}
                          </td>
                          <td className="px-4 py-3">
                            <input
                              aria-label={`Default repository of ${mapping.project}: ${mapping.repo}`}
                              checked={mapping.default}
                              className="size-4 cursor-pointer disabled:cursor-not-allowed"
                              disabled={deleteMapping.isPending || putMapping.isPending}
                              name={`default-repository-${mapping.project}`}
                              onChange={() =>
                                putMapping.mutate({
                                  default: true,
                                  project: mapping.project,
                                  repo: mapping.repo,
                                })
                              }
                              type="radio"
                            />
                          </td>
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
                <label className={settingsFieldLabel} htmlFor="repository">
                  Repository
                  <input
                    className={settingsMonoInput}
                    id="repository"
                    onChange={(event) => setRepository(event.target.value)}
                    pattern={ownerRepoPattern}
                    placeholder="owner/repo"
                    required
                    value={repository}
                  />
                </label>
                <label className={settingsFieldLabel} htmlFor="project">
                  Project
                  <ProjectSelect
                    id="project"
                    onChange={setProject}
                    projects={projects.data}
                    value={project}
                  />
                </label>
                <button
                  className={settingsSubmitButton}
                  disabled={putMapping.isPending || deleteMapping.isPending}
                  type="submit"
                >
                  Add mapping
                </button>
              </form>
              {failedAction !== undefined ? (
                <div className="mt-4">
                  <QueryError
                    message={failedAction.message}
                    onRetry={retry}
                    retrying={putMapping.isPending || deleteMapping.isPending}
                  />
                </div>
              ) : null}
            </>
          ) : null}
        </section>
        <ArchitectureSourcesSection />
        <section aria-labelledby="delivery-timeline-heading">
          <h2
            className={`text-xl font-semibold ${textPrimaryOnCanvas}`}
            id="delivery-timeline-heading"
          >
            Delivery timeline
          </h2>
          <p className={`mt-1 text-sm ${textSecondaryOnCanvas}`}>
            Which repository deploys, and whose merged pull requests the Delivery page counts.
            Saving verifies the Dispatch GitHub App can read the deploy repository.
          </p>
          <DeliverySettingsForm />
        </section>
      </div>
    </section>
  );
}
