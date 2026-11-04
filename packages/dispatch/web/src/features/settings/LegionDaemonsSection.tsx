// The Settings page's Legion section (dispatch://INFRA-1077): which project's state Dispatch
// reads from which daemon. A project's setting is what brings its Legion view at all - the
// Issues page's selector reads this same list (`legionSettingsQuery`, `features/project/
// ProjectPage.tsx`) - so naming a daemon here is how a project gets one, and removing one stops
// Dispatch reading it while everything already read stays.
//
// The two writes are `PUT` and `DELETE /api/v1/settings/legion/{project}`, classed
// `scopeDeployment` beside the repository mappings and the Delivery settings: pointing Dispatch
// at a host is the deployment's business, not one project's. A credential naming one project is
// refused by the server, which is the only role Dispatch has, so the controls are offered to any
// signed-in person (`useIsHuman`) and the server refuses the rest; its refusal is shown under the
// form as it wrote it.
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { type FormEvent, type ReactNode, useState } from "react";

import { api, apiErrorMessage } from "../../api/client";
import { legionSettingsQuery, projectsQuery } from "../../api/queries";
import type { LegionSetting } from "../../api/types";
import { QueryError } from "../../components/QueryError";
import { useIsHuman } from "../../hooks/useIsHuman";
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
import { Timestamp } from "../refs/Timestamp";
import { useKeyedWrite } from "../writes/useKeyedWrite";
import {
  settingsFieldLabel,
  settingsMonoInput,
  settingsSubmitButton,
  settingsTableHead,
  settingsTableRow,
  settingsTableWrapper,
} from "./classes";
import { ProjectSelect } from "./ProjectSelect";

/** A daemon a person named: the project it serves and the base URL its state is read from. */
interface DaemonInput {
  readonly project: string;
  readonly state_url: string;
}

/** A write the server refused or never answered, or a URL the field itself refused, with what
 *  the page says about it: the server's own reason where it gave one, else a generic line. An
 *  `input` refusal is the field's, so no Retry is offered for it - the URL has to change first. */
type FailedAction =
  | ({ readonly kind: "save"; readonly message: string } & DaemonInput)
  | { readonly kind: "remove"; readonly message: string; readonly project: string }
  | { readonly kind: "input"; readonly message: string };

/** The settings with `setting` in place of its project's row, in the order the route lists them
 *  (`ListSettings`, `order by project`), so an optimistic row sits where the next read puts it. */
function withSetting(current: readonly LegionSetting[], setting: LegionSetting): LegionSetting[] {
  return [...current.filter((row) => row.project !== setting.project), setting].sort((a, b) =>
    a.project < b.project ? -1 : a.project > b.project ? 1 : 0
  );
}

/** The daemon base URL a setting may hold, trimmed, or the refusal that names what is wrong with
 *  it - the server's rule and the server's own words, so the field refuses what it would refuse
 *  and says the same thing (`legionStateURL`,
 *  packages/envoy/internal/dispatch/api/settings_legion.go). A URL the server parses differently
 *  is still its call: its 400 shows where this one would. */
export function legionStateUrl(raw: string): { readonly url: string; readonly refusal: string } {
  const trimmed = raw.trim().replace(/\/$/, "");
  if (trimmed === "") {
    return { refusal: "state_url is required", url: "" };
  }
  let parsed: URL;
  try {
    parsed = new URL(trimmed);
  } catch {
    return {
      refusal: "state_url must be an absolute http(s) URL with a host and no path",
      url: "",
    };
  }
  if (
    (parsed.protocol !== "http:" && parsed.protocol !== "https:") ||
    parsed.host === "" ||
    // A URL with no path parses here with `/` for one, since the trailing slash is trimmed above.
    (parsed.pathname !== "" && parsed.pathname !== "/")
  ) {
    return {
      refusal: "state_url must be an absolute http(s) URL with a host and no path",
      url: "",
    };
  }
  if (
    parsed.username !== "" ||
    parsed.password !== "" ||
    trimmed.includes("?") ||
    parsed.hash !== ""
  ) {
    return { refusal: "state_url must carry no user, password, query or fragment", url: "" };
  }
  return { refusal: "", url: trimmed };
}

/** Each project's Legion daemon, one row per setting, and - for a signed-in person - the field
 *  that names one and the Remove that stops reading it. */
export function LegionDaemonsSection(): ReactNode {
  const queryClient = useQueryClient();
  const human = useIsHuman();
  const [project, setProject] = useState("");
  const [stateUrl, setStateUrl] = useState("");
  const [failedAction, setFailedAction] = useState<FailedAction>();
  const settingsKey = legionSettingsQuery().queryKey;
  const settings = useQuery(legionSettingsQuery());
  const projects = useQuery(projectsQuery());
  // Both writes show their refusal under the form, with the row they changed put back.
  const saveDaemon = useKeyedWrite<
    LegionSetting,
    DaemonInput,
    { previous: LegionSetting[] | undefined }
  >({
    write: ({ project: named, state_url: url }, bounds) => api.putLegionSetting(named, url, bounds),
    inline: true,
    label: ({ project: named }) => `${named}'s Legion daemon`,
    onError: (error, variables, context) => {
      queryClient.setQueryData(settingsKey, context?.previous);
      setFailedAction({
        kind: "save",
        message: apiErrorMessage(error, `Couldn't save ${variables.project}'s Legion daemon.`),
        ...variables,
      });
    },
    onMutate: async (variables) => {
      await queryClient.cancelQueries({ queryKey: settingsKey });
      const previous = queryClient.getQueryData<LegionSetting[]>(settingsKey);
      const existing = previous?.find((row) => row.project === variables.project);
      queryClient.setQueryData<LegionSetting[]>(settingsKey, (current = []) =>
        withSetting(current, {
          // As the server keeps it: whoever first named this project's daemon, else this person.
          created_by: existing?.created_by ?? { id: "you", kind: "user" },
          project: variables.project,
          state_url: variables.state_url,
          updated_at: new Date().toISOString(),
        })
      );
      return { previous };
    },
    // A landing with no answer keeps the optimistic row; `onSettled` reads them all again.
    onSuccess: (setting) => {
      if (setting !== null) {
        queryClient.setQueryData<LegionSetting[]>(settingsKey, (current = []) =>
          withSetting(current, setting)
        );
      }
      setFailedAction(undefined);
      setProject("");
      setStateUrl("");
    },
    // The answer is one row as the server saved it, not the deployment's settings when it
    // arrives: another browser may have named another project's daemon meanwhile. Reading them
    // again once the write settles leaves the page on the server's state, as the repository
    // mappings do.
    onSettled: () => queryClient.invalidateQueries({ queryKey: settingsKey }),
  });
  const removeDaemon = useKeyedWrite<void, string, { previous: LegionSetting[] | undefined }>({
    write: (named: string, bounds) => api.deleteLegionSetting(named, bounds),
    inline: true,
    label: (named) => `${named}'s Legion daemon removed`,
    onError: (error, named, context) => {
      queryClient.setQueryData(settingsKey, context?.previous);
      setFailedAction({
        kind: "remove",
        message: apiErrorMessage(error, `Couldn't remove ${named}'s Legion daemon.`),
        project: named,
      });
    },
    onMutate: async (named) => {
      await queryClient.cancelQueries({ queryKey: settingsKey });
      const previous = queryClient.getQueryData<LegionSetting[]>(settingsKey);
      queryClient.setQueryData<LegionSetting[]>(settingsKey, (current = []) =>
        current.filter((row) => row.project !== named)
      );
      return { previous };
    },
    onSuccess: () => setFailedAction(undefined),
    onSettled: () => queryClient.invalidateQueries({ queryKey: settingsKey }),
  });

  const retry = () => {
    if (failedAction?.kind === "save") {
      const { kind: _kind, message: _message, ...variables } = failedAction;
      saveDaemon.mutate(variables);
      return;
    }
    if (failedAction?.kind === "remove") {
      removeDaemon.mutate(failedAction.project);
    }
  };
  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const { refusal, url } = legionStateUrl(stateUrl);
    if (refusal !== "") {
      setFailedAction({ kind: "input", message: refusal });
      return;
    }
    saveDaemon.mutate({ project, state_url: url });
  };
  const writing = saveDaemon.isPending || removeDaemon.isPending;

  return (
    <section aria-labelledby="legion-daemons-heading" data-legion-daemons>
      <h2 className={`text-xl font-semibold ${textPrimaryOnCanvas}`} id="legion-daemons-heading">
        Legion
      </h2>
      <p className={`mt-1 text-sm ${textSecondaryOnCanvas}`}>
        A project whose Legion daemon is named here has a Legion view on its Issues page, drawn from
        what Dispatch read of that daemon. The address is the daemon&apos;s base URL, with no path:
        Dispatch reads <code>/legion/v1/state</code> under it. Removing one stops the reading; what
        was read stays.
        {human ? "" : " Sign in to name one."}
      </p>

      {settings.isPending || projects.isPending ? (
        <p className={`mt-6 ${textMutedOnCanvas}`}>Loading Legion daemons…</p>
      ) : null}
      {settings.isError ? (
        <div className="mt-6">
          <QueryError
            message="Couldn't load the Legion daemons."
            onRetry={() => void settings.refetch()}
            retrying={settings.isFetching}
          />
        </div>
      ) : null}
      {settings.isSuccess && projects.isSuccess ? (
        <>
          <div className={settingsTableWrapper}>
            <table className="w-full text-left text-sm">
              <thead className={settingsTableHead}>
                <tr>
                  <th className="px-4 py-3 font-semibold" scope="col">
                    Project
                  </th>
                  <th className="px-4 py-3 font-semibold" scope="col">
                    Daemon state URL
                  </th>
                  <th className="px-4 py-3 font-semibold" scope="col">
                    Changed
                  </th>
                  {human ? (
                    <th className="px-4 py-3" scope="col">
                      <span className="sr-only">Actions</span>
                    </th>
                  ) : null}
                </tr>
              </thead>
              <tbody>
                {settings.data.length === 0 ? (
                  <tr>
                    <td
                      className={`px-4 py-5 ${textMutedOnSurface}`}
                      colSpan={human ? 4 : 3}
                      data-legion-daemons-empty
                    >
                      No project names a Legion daemon yet.
                    </td>
                  </tr>
                ) : (
                  settings.data.map((setting) => (
                    <tr
                      className={settingsTableRow}
                      data-legion-daemon={setting.project}
                      key={setting.project}
                    >
                      <td className={`px-4 py-3 font-mono ${textPrimaryOnSurface}`}>
                        {setting.project}
                      </td>
                      <td
                        className={`px-4 py-3 font-mono ${textSecondaryOnSurface}`}
                        data-legion-daemon-url
                      >
                        {setting.state_url}
                      </td>
                      <td className={`px-4 py-3 ${textSecondaryOnSurface}`}>
                        <Timestamp at={setting.updated_at} className={textMutedOnSurface} />
                      </td>
                      {human ? (
                        <td className="px-4 py-3 text-right">
                          <button
                            aria-label={`Remove the Legion daemon of ${setting.project}`}
                            className={`font-medium disabled:cursor-not-allowed disabled:opacity-50 ${dangerText} ${dangerHoverText}`}
                            disabled={writing}
                            onClick={() => removeDaemon.mutate(setting.project)}
                            type="button"
                          >
                            Remove
                          </button>
                        </td>
                      ) : null}
                    </tr>
                  ))
                )}
              </tbody>
            </table>
          </div>

          {human ? (
            <form
              className={`mt-6 grid gap-4 rounded-xl border p-4 sm:grid-cols-[1fr_1fr_auto] sm:items-end ${borderDefault}`}
              onSubmit={submit}
            >
              <label className={settingsFieldLabel} htmlFor="legion-project">
                Legion project
                <ProjectSelect
                  disabled={writing}
                  id="legion-project"
                  onChange={setProject}
                  projects={projects.data}
                  value={project}
                />
              </label>
              <label className={settingsFieldLabel} htmlFor="legion-state-url">
                Legion daemon state URL
                <input
                  className={settingsMonoInput}
                  disabled={writing}
                  id="legion-state-url"
                  onChange={(event) => setStateUrl(event.target.value)}
                  placeholder="https://daemon.example"
                  required
                  value={stateUrl}
                />
              </label>
              <button className={settingsSubmitButton} disabled={writing} type="submit">
                Save Legion daemon
              </button>
            </form>
          ) : null}
          {failedAction !== undefined ? (
            <div className="mt-4">
              <QueryError
                message={failedAction.message}
                onRetry={failedAction.kind === "input" ? undefined : retry}
                retrying={writing}
              />
            </div>
          ) : null}
        </>
      ) : null}
    </section>
  );
}
