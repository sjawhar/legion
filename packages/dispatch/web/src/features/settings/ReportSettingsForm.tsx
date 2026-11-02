import { type UseQueryResult, useQuery, useQueryClient } from "@tanstack/react-query";
import { type FormEvent, type ReactNode, useId, useState } from "react";

import { api, apiErrorMessage } from "../../api/client";
import { projectsQuery, reportSettingsQuery } from "../../api/queries";
import type { Project, ReportSettings, ReportSettingsInput } from "../../api/types";
import { QueryError } from "../../components/QueryError";
import { useSubmitGuard } from "../../hooks/useSubmitGuard";
import { borderDefault, textMutedOnCanvas } from "../../theme/classes";
import { useKeyedWrite } from "../writes/useKeyedWrite";
import { settingsFieldLabel, settingsMonoInput, settingsSubmitButton } from "./classes";

/**
 * Where **Report a problem** sends a report: the project its issue is filed in, and the role of
 * the agent that fixes one when the reporter asks. With no project the report form says reports
 * are not set up; with no role it offers no agent. Save replaces both; the server refuses a
 * project that does not exist and an agent named by anything but a role, and the refusal shows
 * under the form as the server wrote it.
 */
export function ReportSettingsForm(): ReactNode {
  const settings = useQuery(reportSettingsQuery());
  // Read with the page, beside every other section's read of the same list, so they share one
  // request rather than the fields asking again once the settings land.
  const projects = useQuery(projectsQuery());
  if (settings.isPending) {
    return <p className={`mt-6 ${textMutedOnCanvas}`}>Loading report settings…</p>;
  }
  if (settings.isError) {
    return (
      <div className="mt-6">
        <QueryError
          message="Couldn't load the report settings."
          onRetry={() => void settings.refetch()}
          retrying={settings.isFetching}
        />
      </div>
    );
  }
  // Keyed by the record's write time, so a save starts the fields over from what was stored.
  return (
    <ReportSettingsFields
      key={settings.data.updated_at ?? "unset"}
      projects={projects}
      record={settings.data}
    />
  );
}

function ReportSettingsFields({
  projects,
  record,
}: {
  projects: UseQueryResult<Project[]>;
  record: ReportSettings;
}): ReactNode {
  const queryClient = useQueryClient();
  const id = useId();
  const submitGuard = useSubmitGuard();
  const [project, setProject] = useState(record.project ?? "");
  const [agentRoute, setAgentRoute] = useState(record.agent_route ?? "");
  const save = useKeyedWrite({
    write: (input: ReportSettingsInput, bounds) => api.putReportSettings(input, bounds),
    inline: true,
    label: () => "Report settings saved",
    onSettled: () => submitGuard.release(),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: reportSettingsQuery().queryKey });
    },
  });
  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const route = agentRoute.trim();
    submitGuard.guard(() =>
      save.mutate({
        agent_route: route === "" ? null : route,
        project: project === "" ? null : project,
      })
    );
  };

  return (
    <>
      <form className={`mt-6 grid gap-4 rounded-xl border p-4 ${borderDefault}`} onSubmit={submit}>
        <fieldset className="grid min-w-0 gap-4 sm:grid-cols-2" disabled={save.isPending}>
          <div className="grid gap-1">
            <label className={settingsFieldLabel} htmlFor={`${id}-project`}>
              Project for reports
              <select
                aria-describedby={`${id}-project-hint`}
                className={settingsMonoInput}
                id={`${id}-project`}
                onChange={(event) => setProject(event.target.value)}
                value={project}
              >
                <option value="">Not set: reports are off</option>
                {(projects.data ?? []).map((candidate) => (
                  <option key={candidate.key} value={candidate.key}>
                    {candidate.key} · {candidate.name}
                  </option>
                ))}
              </select>
            </label>
            <p className={`text-xs ${textMutedOnCanvas}`} id={`${id}-project-hint`}>
              Each report becomes an issue here, labelled report.
            </p>
          </div>
          <div className="grid gap-1">
            <label className={settingsFieldLabel} htmlFor={`${id}-agent-route`}>
              Agent role
              <input
                aria-describedby={`${id}-agent-route-hint`}
                className={settingsMonoInput}
                id={`${id}-agent-route`}
                onChange={(event) => setAgentRoute(event.target.value)}
                placeholder="role:<name> (optional)"
                value={agentRoute}
              />
            </label>
            <p className={`text-xs ${textMutedOnCanvas}`} id={`${id}-agent-route-hint`}>
              The role the agent that fixes reports holds. A reporter who ticks Ask an agent to fix
              this sends the issue to whichever session holds it. Leave empty to offer no agent.
            </p>
          </div>
        </fieldset>
        {projects.isError ? (
          <QueryError
            message="Couldn't load the projects."
            onRetry={() => void projects.refetch()}
            retrying={projects.isFetching}
          />
        ) : null}
        <button
          className={`justify-self-start ${settingsSubmitButton}`}
          disabled={save.isPending}
          type="submit"
        >
          Save report settings
        </button>
      </form>
      {save.isError ? (
        <div className="mt-4">
          <QueryError message={apiErrorMessage(save.error, "Couldn't save the report settings.")} />
        </div>
      ) : null}
    </>
  );
}
