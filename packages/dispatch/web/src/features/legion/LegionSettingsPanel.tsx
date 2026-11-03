// The Legion view's settings panel: the daemon's own `legion.yaml`, as it reports it, with where
// each value came from — its configuration file, or a change someone made here, with who made it
// and when. Dispatch models none of the values: they ride from the daemon as JSON and go back as
// JSON (`GET /api/v1/legion/board`'s `settings` and `setting_sources`).
//
// A change goes to `PUT /api/v1/legion/{project}/settings`, which relays it to the daemon with the
// pod's projected ServiceAccount token and the caller's login, and answers the daemon's own
// verdict for each key: applied at once, by rebuilding the client or listener that held it, or at
// the next worker launch — or refused, with the daemon's reason. Dispatch re-reads none of that,
// so neither does this panel: it shows the verdict it was given.
//
// Who may change one: `scopeDeployment` refuses a caller whose credential names one project, which
// is the only role Dispatch has, so the control is offered to any signed-in person (`useIsHuman`)
// and the server refuses the rest — the way `GrantsSection` and the Delivery settings gate. A
// refusal lands on the row it was made from.
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { type FormEvent, type ReactNode, useId, useState } from "react";

import { api, apiErrorMessage } from "../../api/client";
import { legionBoardQuery, whoAmIQuery } from "../../api/queries";
import type {
  LegionBoardResponse,
  LegionSettingSource,
  LegionSettingVerdict,
} from "../../api/types";
import { useIsHuman } from "../../hooks/useIsHuman";
import {
  dangerText,
  successText,
  textMutedOnCanvas,
  textPrimaryOnCanvas,
  textPrimaryOnSurface,
  textSecondaryOnCanvas,
  textSecondaryOnSurface,
} from "../../theme/classes";
import {
  settingsMonoInput,
  settingsSubmitButton,
  settingsTableHead,
  settingsTableRow,
  settingsTableWrapper,
} from "../settings/classes";
import { useKeyedWrite } from "../writes/useKeyedWrite";
import { describeSettingSource, describeVerdict, formatSettingValue } from "./lib/format";

/** What one key of a change ended as, on the row it was made from: the daemon's verdict, a
 *  refusal Dispatch itself made (no daemon, the daemon unreachable, a credential naming one
 *  project), or an answer Dispatch could not keep. */
type RowResult =
  | { readonly kind: "verdict"; readonly verdict: LegionSettingVerdict }
  | { readonly kind: "failed"; readonly message: string }
  | { readonly kind: "unanswered" };

/** One key's change: the dotted `legion.yaml` path and the value the daemon is asked to take. */
interface SettingChange {
  readonly key: string;
  readonly value: unknown;
}

/** `entries` with `key` taking `value`, or without it where `value` is undefined: a key the
 *  daemon reports no value for is one the panel must not invent one for. */
function withEntry<V>(
  entries: Readonly<Record<string, V>>,
  key: string,
  value: V | undefined
): Readonly<Record<string, V>> {
  const { [key]: _dropped, ...rest } = entries;
  return value === undefined ? rest : { ...rest, [key]: value };
}

/** The board as it stands with `key` taking `value` from this person, now: what the panel shows
 *  until the next read of the daemon confirms it. The daemon has already said it applied the
 *  change, and Dispatch learns the daemon's own `settingSources` only at its next pass. */
function withChangedSetting(
  board: LegionBoardResponse,
  key: string,
  value: unknown,
  by: string | null
): LegionBoardResponse {
  const source: LegionSettingSource = { source: "route", by, at: new Date().toISOString() };
  return {
    ...board,
    settings: { ...board.settings, [key]: value },
    setting_sources: { ...board.setting_sources, [key]: source },
  };
}

/** The board with `key` back as `previous` showed it, and every other field as it stands now: a
 *  change that was refused, or that failed, puts back the one key it touched and not a board a
 *  live delta has moved on since. */
function withSettingAsItWas(
  board: LegionBoardResponse,
  key: string,
  previous: LegionBoardResponse | undefined
): LegionBoardResponse {
  return {
    ...board,
    settings: withEntry(board.settings, key, previous?.settings[key]),
    setting_sources: withEntry(board.setting_sources, key, previous?.setting_sources[key]),
  };
}

/** The daemon's settings, each with its value and where that value came from, and — for a signed
 *  in person — a control that changes it. `project` is whose daemon these are; `settings` and
 *  `sources` are the board's own two blocks, which an applied change writes back into through the
 *  board's cache entry. */
export function LegionSettingsPanel({
  project,
  settings,
  sources,
}: {
  readonly project: string;
  readonly settings: Readonly<Record<string, unknown>>;
  readonly sources: Readonly<Record<string, LegionSettingSource>>;
}): ReactNode {
  const headingId = useId();
  const queryClient = useQueryClient();
  const human = useIsHuman();
  const viewer = useQuery(whoAmIQuery()).data?.login ?? null;
  const boardKey = legionBoardQuery({ kind: "legion", project }).queryKey;
  // The value each row's input holds while it is being edited; a row nobody has typed in shows
  // the daemon's own value.
  const [drafts, setDrafts] = useState<Readonly<Record<string, string>>>({});
  const [results, setResults] = useState<Readonly<Record<string, RowResult>>>({});
  /** Puts `key` back as `previous` showed it: a change refused or failed puts back the one key it
   *  touched, leaving every other field as the live subscription has it now. */
  const rollBack = (key: string, previous: LegionBoardResponse | undefined) => {
    queryClient.setQueryData<LegionBoardResponse>(boardKey, (board) =>
      board === undefined ? board : withSettingAsItWas(board, key, previous)
    );
  };

  const change = useKeyedWrite<
    Readonly<Record<string, LegionSettingVerdict>>,
    SettingChange,
    { readonly previous: LegionBoardResponse | undefined }
  >({
    write: ({ key, value }, bounds) =>
      api.putLegionProjectSettings(project, { [key]: value }, bounds),
    inline: true,
    label: ({ key }) => `${key} changed on ${project}'s Legion daemon`,
    onError: (error, variables, context) => {
      rollBack(variables.key, context?.previous);
      setResults((current) => ({
        ...current,
        [variables.key]: {
          kind: "failed",
          message: apiErrorMessage(error, `Couldn't change ${variables.key}.`),
        },
      }));
    },
    onMutate: async (variables) => {
      await queryClient.cancelQueries({ queryKey: boardKey });
      const previous = queryClient.getQueryData<LegionBoardResponse>(boardKey);
      if (previous !== undefined) {
        queryClient.setQueryData(
          boardKey,
          withChangedSetting(previous, variables.key, variables.value, viewer)
        );
      }
      setResults((current) => {
        const { [variables.key]: _cleared, ...rest } = current;
        return rest;
      });
      return { previous };
    },
    // A landing Dispatch could not keep the answer of leaves the optimistic row: the daemon may
    // well have applied it, and the next read of the daemon is what settles it.
    onSuccess: (answer, variables, context) => {
      if (answer === null) {
        setResults((current) => ({ ...current, [variables.key]: { kind: "unanswered" } }));
        return;
      }
      const verdict = answer[variables.key] ?? {};
      // A key the daemon refused never changed, so the row goes back to the value it showed.
      if (verdict.refused !== undefined) {
        rollBack(variables.key, context?.previous);
      }
      setResults((current) => ({ ...current, [variables.key]: { kind: "verdict", verdict } }));
      setDrafts((current) => {
        const { [variables.key]: _applied, ...rest } = current;
        return rest;
      });
    },
    // No read follows: the board is live (`api/live-socket.ts`), so the daemon's next pass
    // replaces `settings` and `setting_sources` whole on this same subscription. Invalidating a
    // live query here would drop the subscription's own answer for one nothing re-reads.
  });

  // Every settable path, the ones the daemon withholds a value for included: a credential's
  // provenance is reported with no value anywhere, which is how a rotation is visible.
  const keys = [...new Set([...Object.keys(settings), ...Object.keys(sources)])].sort();

  const submit = (event: FormEvent<HTMLFormElement>, key: string) => {
    event.preventDefault();
    const typed = drafts[key] ?? formatSettingValue(settings[key]);
    let value: unknown;
    try {
      value = JSON.parse(typed);
    } catch {
      setResults((current) => ({
        ...current,
        [key]: {
          kind: "failed",
          message: 'That is not JSON. A setting\'s value is written as JSON: 4, "legion", ["a"].',
        },
      }));
      return;
    }
    change.mutate({ key, value });
  };

  return (
    <section aria-labelledby={headingId} className="mt-6" data-legion-settings>
      <h3 className={`text-base font-semibold ${textPrimaryOnCanvas}`} id={headingId}>
        Daemon settings
      </h3>
      <p className={`mt-1 text-sm ${textSecondaryOnCanvas}`}>
        {project}&apos;s <code>legion.yaml</code> as the daemon reports it, with where each value
        came from. A change reaches the running daemon at once, and the daemon decides each setting
        and says how it took it.
        {human ? "" : " Sign in to change one."}
      </p>
      {keys.length === 0 ? (
        <p className={`mt-4 text-sm ${textMutedOnCanvas}`} data-legion-settings-empty>
          No read of this daemon has recorded its settings yet.
        </p>
      ) : (
        <div className={settingsTableWrapper}>
          <table className="w-full text-left text-sm">
            <thead className={settingsTableHead}>
              <tr>
                <th className="px-3 py-2 font-medium" scope="col">
                  Setting
                </th>
                <th className="px-3 py-2 font-medium" scope="col">
                  Value
                </th>
                <th className="px-3 py-2 font-medium" scope="col">
                  Where it came from
                </th>
                {human ? (
                  <th className="px-3 py-2 font-medium" scope="col">
                    Change it
                  </th>
                ) : null}
              </tr>
            </thead>
            <tbody>
              {keys.map((key) => {
                const value = formatSettingValue(settings[key]);
                const source = sources[key];
                const result = results[key];
                return (
                  <tr className={settingsTableRow} data-legion-setting={key} key={key}>
                    <th
                      className={`px-3 py-2 text-left font-mono font-normal ${textPrimaryOnSurface}`}
                      scope="row"
                    >
                      {key}
                    </th>
                    <td
                      className={`px-3 py-2 font-mono ${textPrimaryOnSurface}`}
                      data-legion-setting-value={key}
                    >
                      {value}
                    </td>
                    <td
                      className={`px-3 py-2 ${textSecondaryOnSurface}`}
                      data-legion-setting-source={source?.source ?? "unknown"}
                    >
                      {describeSettingSource(source)}
                    </td>
                    {human ? (
                      <td className="px-3 py-2">
                        <form
                          className="flex flex-wrap items-center gap-2"
                          onSubmit={(event) => submit(event, key)}
                        >
                          <input
                            aria-label={`New value for ${key}`}
                            className={`${settingsMonoInput} w-40 text-sm`}
                            onChange={(event) =>
                              setDrafts((current) => ({ ...current, [key]: event.target.value }))
                            }
                            value={drafts[key] ?? value}
                          />
                          <button
                            className={`${settingsSubmitButton} text-sm`}
                            disabled={change.isPending}
                            type="submit"
                          >
                            Change {key}
                          </button>
                          {result === undefined ? null : (
                            <span data-legion-setting-result={key}>
                              <RowAnswer result={result} />
                            </span>
                          )}
                        </form>
                      </td>
                    ) : null}
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}
    </section>
  );
}

/** What the daemon, or Dispatch, said about this row's change. */
function RowAnswer({ result }: { readonly result: RowResult }): ReactNode {
  if (result.kind === "failed") {
    return (
      <span className={`text-xs ${dangerText}`} role="alert">
        {result.message}
      </span>
    );
  }
  if (result.kind === "unanswered") {
    return (
      <span className={`text-xs ${textSecondaryOnSurface}`} role="status">
        Dispatch did not keep the daemon&apos;s answer; the next read of the daemon shows what it
        did.
      </span>
    );
  }
  const { refused, text } = describeVerdict(result.verdict);
  return (
    <span
      className={`text-xs ${refused ? dangerText : successText}`}
      role={refused ? "alert" : "status"}
    >
      {text}
    </span>
  );
}
