import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { type FormEvent, type ReactNode, useId, useState } from "react";

import { api, apiErrorMessage } from "../../api/client";
import { deliverySettingsQuery } from "../../api/queries";
import type { DeliverySettings, DeliverySettingsInput } from "../../api/types";
import { QueryError } from "../../components/QueryError";
import { useSubmitGuard } from "../../hooks/useSubmitGuard";
import { borderDefault, textMutedOnCanvas } from "../../theme/classes";
import {
  ownerRepoPattern,
  settingsFieldLabel,
  settingsMonoInput,
  settingsSubmitButton,
} from "../settings/classes";

/** The characters that separate the entries of a list field, as a character class's body: line
 *  breaks, and commas or spaces too, since neither a GitHub login nor an `owner/repo` can hold
 *  one. */
const SEPARATOR_CHARACTERS = String.raw`\s,`;

/** A run of separators between two entries. */
const LIST_SEPARATOR = new RegExp(`[${SEPARATOR_CHARACTERS}]+`);

/** Any one character of a list entry: anything but a separator. */
const LIST_ENTRY_CHARACTER = new RegExp(`[^${SEPARATOR_CHARACTERS}]`);

/** The entries a list field's text holds. */
function listEntries(text: string): string[] {
  return text.split(LIST_SEPARATOR).filter(Boolean);
}

/** A required field's text as the draft keeps it: text holding nothing but whitespace is empty,
 *  so the browser's `required` check refuses it before the server's 400 would. */
function textOrEmpty(text: string): string {
  return text.trim() === "" ? "" : text;
}

/** A required list field's text as the draft keeps it: text holding no entry, only separators, is
 *  empty, as `textOrEmpty` keeps a field of only whitespace. */
function listOrEmpty(text: string): string {
  return LIST_ENTRY_CHARACTER.test(text) ? text : "";
}

/** The fields as typed: the two lists are the textareas' text, split only when saved. */
interface Draft {
  deploy_repo: string;
  deploy_workflow_path: string;
  production_job_name: string;
  pr_checks_workflow_path: string;
  population_authors: string;
  excluded_repos: string;
}

interface DeliverySettingsFormProps {
  /** The record when the caller already holds the answer, and the form then reads nothing: the
   *  Delivery page passes `null`, since the timeline's `DELIVERY_NOT_CONFIGURED` already says no
   *  record is stored. Left out, the form reads `GET /api/v1/settings/delivery`. */
  readonly initial?: DeliverySettings | null;
}

/**
 * The delivery timeline's configuration record as a form: the deploy repository, its deploy and
 * pull-request-check workflow paths, the deploy workflow's production job, and the population's
 * authors and excluded repositories. The two lists take one entry per line (a comma or a space
 * also separates entries). The fields start from the stored record (`initial`, or the record the
 * form reads), and empty, with placeholders saying what each holds, when there is none. A required
 * field holding only whitespace (a list, only separators) is empty, so the browser refuses it
 * before any request. Save replaces the whole record; the server first checks that the GitHub App
 * can read the deploy repository, and a refusal shows under the form as the server wrote it. A
 * save refreshes the record and every delivery timeline read, so the Delivery page, which renders
 * this form in place of a timeline it cannot read yet, shows the timeline once the save succeeds.
 */
export function DeliverySettingsForm({ initial }: DeliverySettingsFormProps): ReactNode {
  const settings = useQuery({ ...deliverySettingsQuery(), enabled: initial === undefined });
  let record = initial;
  if (record === undefined) {
    if (settings.isPending) {
      return <p className={`mt-6 ${textMutedOnCanvas}`}>Loading delivery settings…</p>;
    }
    if (settings.isError) {
      return (
        <div className="mt-6">
          <QueryError
            message="Couldn't load the delivery settings."
            onRetry={() => void settings.refetch()}
            retrying={settings.isFetching}
          />
        </div>
      );
    }
    record = settings.data;
  }
  // Keyed by the record's write time, so a save (anyone's) starts the fields over from the record
  // as the server stored it, and so does a caller handing over another record.
  return <DeliverySettingsFields key={record?.updated_at ?? "unset"} record={record} />;
}

function DeliverySettingsFields({ record }: { record: DeliverySettings | null }): ReactNode {
  const queryClient = useQueryClient();
  const id = useId();
  const submitGuard = useSubmitGuard();
  const [draft, setDraft] = useState<Draft>(() => ({
    deploy_repo: record?.deploy_repo ?? "",
    deploy_workflow_path: record?.deploy_workflow_path ?? "",
    excluded_repos: record?.excluded_repos.join("\n") ?? "",
    population_authors: record?.population_authors.join("\n") ?? "",
    pr_checks_workflow_path: record?.pr_checks_workflow_path ?? "",
    production_job_name: record?.production_job_name ?? "",
  }));
  const save = useMutation({
    mutationFn: (input: DeliverySettingsInput) => api.putDeliverySettings(input),
    onSettled: () => submitGuard.release(),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: deliverySettingsQuery().queryKey });
      void queryClient.invalidateQueries({ queryKey: ["delivery"] });
    },
  });
  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    submitGuard.guard(() =>
      save.mutate({
        deploy_repo: draft.deploy_repo,
        deploy_workflow_path: draft.deploy_workflow_path.trim(),
        excluded_repos: listEntries(draft.excluded_repos),
        population_authors: listEntries(draft.population_authors),
        pr_checks_workflow_path: draft.pr_checks_workflow_path.trim(),
        production_job_name: draft.production_job_name.trim(),
      })
    );
  };

  return (
    <>
      <form className={`mt-6 grid gap-4 rounded-xl border p-4 ${borderDefault}`} onSubmit={submit}>
        <fieldset className="grid min-w-0 gap-4 sm:grid-cols-2" disabled={save.isPending}>
          <label className={settingsFieldLabel} htmlFor={`${id}-deploy-repo`}>
            Deploy repository
            <input
              className={settingsMonoInput}
              id={`${id}-deploy-repo`}
              onChange={(event) => setDraft({ ...draft, deploy_repo: event.target.value })}
              pattern={ownerRepoPattern}
              placeholder="owner/repo"
              required
              value={draft.deploy_repo}
            />
          </label>
          <label className={settingsFieldLabel} htmlFor={`${id}-deploy-workflow`}>
            Deploy workflow
            <input
              className={settingsMonoInput}
              id={`${id}-deploy-workflow`}
              onChange={(event) =>
                setDraft({ ...draft, deploy_workflow_path: textOrEmpty(event.target.value) })
              }
              placeholder=".github/workflows/<deploy>.yml"
              required
              value={draft.deploy_workflow_path}
            />
          </label>
          <div className="grid gap-1">
            <label className={settingsFieldLabel} htmlFor={`${id}-production-job`}>
              Production job
              <input
                aria-describedby={`${id}-production-job-hint`}
                className={settingsMonoInput}
                id={`${id}-production-job`}
                onChange={(event) =>
                  setDraft({ ...draft, production_job_name: textOrEmpty(event.target.value) })
                }
                placeholder="The job's name as a deploy run lists it"
                required
                value={draft.production_job_name}
              />
            </label>
            <p className={`text-xs ${textMutedOnCanvas}`} id={`${id}-production-job-hint`}>
              The job whose success counts as a production deploy, named as a run lists it: a
              reusable workflow's job reads &lt;caller&gt; / &lt;job&gt;.
            </p>
          </div>
          <label className={settingsFieldLabel} htmlFor={`${id}-pr-checks-workflow`}>
            PR checks workflow
            <input
              className={settingsMonoInput}
              id={`${id}-pr-checks-workflow`}
              onChange={(event) =>
                setDraft({ ...draft, pr_checks_workflow_path: textOrEmpty(event.target.value) })
              }
              placeholder=".github/workflows/<checks>.yml"
              required
              value={draft.pr_checks_workflow_path}
            />
          </label>
          <div className="grid gap-1">
            <label className={settingsFieldLabel} htmlFor={`${id}-population-authors`}>
              Population authors
              <textarea
                aria-describedby={`${id}-population-authors-hint`}
                className={settingsMonoInput}
                id={`${id}-population-authors`}
                onChange={(event) =>
                  setDraft({ ...draft, population_authors: listOrEmpty(event.target.value) })
                }
                placeholder="One GitHub login per line"
                required
                rows={4}
                value={draft.population_authors}
              />
            </label>
            <p className={`text-xs ${textMutedOnCanvas}`} id={`${id}-population-authors-hint`}>
              Whose merged pull requests count, as GitHub spells each login: an App's ends in [bot].
            </p>
          </div>
          <div className="grid gap-1">
            <label className={settingsFieldLabel} htmlFor={`${id}-excluded-repos`}>
              Excluded repositories
              <textarea
                aria-describedby={`${id}-excluded-repos-hint`}
                className={settingsMonoInput}
                id={`${id}-excluded-repos`}
                onChange={(event) => setDraft({ ...draft, excluded_repos: event.target.value })}
                placeholder="One owner/repo per line"
                rows={4}
                value={draft.excluded_repos}
              />
            </label>
            <p className={`text-xs ${textMutedOnCanvas}`} id={`${id}-excluded-repos-hint`}>
              Repositories whose merged pull requests never count, as GitHub spells each owner/repo,
              capitals included.
            </p>
          </div>
        </fieldset>
        <button
          className={`justify-self-start ${settingsSubmitButton}`}
          disabled={save.isPending}
          type="submit"
        >
          Save delivery settings
        </button>
      </form>
      {save.isError ? (
        <div className="mt-4">
          <QueryError
            message={apiErrorMessage(save.error, "Couldn't save the delivery settings.")}
          />
        </div>
      ) : null}
    </>
  );
}
