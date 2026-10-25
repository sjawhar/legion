import { useQuery, useQueryClient } from "@tanstack/react-query";
import { type FormEvent, type ReactNode, useState } from "react";

import { api } from "../../api/client";
import type { AgentToken, CreatedAgentToken } from "../../api/types";
import { QueryError } from "../../components/QueryError";
import { useCopyFeedback } from "../../hooks/useCopyFeedback";
import { compareTimestamps } from "../../lib/timestamps";
import {
  borderDefault,
  card,
  dangerHoverText,
  dangerText,
  inputClasses,
  primaryButtonBg,
  primaryButtonHoverBg,
  successText,
  surfaceMutedBg,
  textMutedOnCanvas,
  textMutedOnSurface,
  textPrimaryOnCanvas,
  textPrimaryOnSurface,
  textSecondaryOnCanvas,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { Timestamp } from "../refs/Timestamp";
import { LandedWithoutAnswer } from "../writes/LandedWithoutAnswer";
import { useKeyedWrite } from "../writes/useKeyedWrite";
import {
  settingsSubmitButton,
  settingsTableHead,
  settingsTableRow,
  settingsTableWrapper,
} from "./classes";

const agentTokensQueryKey = ["agent-tokens"] as const;

/** The live token labelled `name` that a mint whose answer was lost made: the newest of that label,
 *  since the mint is the latest thing to have made one. */
function lostToken(
  tokens: readonly AgentToken[] | undefined,
  name: string
): AgentToken | undefined {
  return tokens
    ?.filter((token) => token.name === name && token.revoked_at === null)
    .reduce<AgentToken | undefined>(
      (newest, token) =>
        newest === undefined || compareTimestamps(token.created_at, newest.created_at) > 0
          ? token
          : newest,
      undefined
    );
}

export function AgentTokensSection(): ReactNode {
  const queryClient = useQueryClient();
  const [name, setName] = useState("");
  const [shownToken, setShownToken] = useState<CreatedAgentToken>();
  /** The label of a token Dispatch minted whose value was lost with its answer, until it is
   *  replaced or the reader writes another label. */
  const [lostLabel, setLostLabel] = useState<string | null>(null);
  const { copy, reset: resetCopyStatus, status: copyStatus } = useCopyFeedback();
  const tokens = useQuery({ queryKey: agentTokensQueryKey, queryFn: () => api.listAgentTokens() });
  // The form shows a refused mint beside the name it was refused over.
  const createToken = useKeyedWrite({
    inline: true,
    label: (input: { name: string }) => `New agent token ${input.name}`,
    write: (input, bounds) => api.createAgentToken(input, bounds),
    onLanded: (created, input) => {
      setName("");
      if (created === null) {
        // Minted, and its value - shown once only - is gone with the answer: the token is
        // listed once the list is read again, and the reader can replace it.
        setShownToken(undefined);
        setLostLabel(input.name);
        void queryClient.invalidateQueries({ queryKey: agentTokensQueryKey });
        return;
      }
      const { token, ...safeToken } = created;
      queryClient.setQueryData<AgentToken[]>(agentTokensQueryKey, (current = []) => [
        safeToken,
        ...current,
      ]);
      setLostLabel(null);
      setShownToken(created);
      resetCopyStatus();
    },
  });
  const revokeToken = useKeyedWrite({
    label: (id: string) => `Agent token ${id} revoked`,
    write: (id: string, bounds) => api.revokeAgentToken(id, bounds),
    onSuccess: (_result, id) => {
      queryClient.setQueryData<AgentToken[]>(agentTokensQueryKey, (current = []) =>
        current.map((token) =>
          token.id === id ? { ...token, revoked_at: new Date().toISOString() } : token
        )
      );
    },
  });
  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    // An emptied form - a mint that landed - sends nothing, whatever submits it.
    if (name.trim() === "") return;
    createToken.mutate({ name });
  };
  const lost = lostLabel === null ? undefined : lostToken(tokens.data, lostLabel);
  /** Revokes the token whose value was lost, then mints another under its label. */
  const replaceLost = () => {
    if (lost === undefined || lostLabel === null) return;
    const label = lostLabel;
    revokeToken.mutate(lost.id, { onSuccess: () => createToken.mutate({ name: label }) });
  };
  const copyToken = () => {
    if (shownToken === undefined) {
      return;
    }
    copy(shownToken.token);
  };
  const revoke = (token: AgentToken) => {
    if (
      window.confirm(`Revoke ${token.name}? Agents using this token will stop working immediately.`)
    ) {
      revokeToken.mutate(token.id);
    }
  };

  return (
    <section aria-labelledby="agent-tokens-heading">
      <h2 className={`text-xl font-semibold ${textPrimaryOnCanvas}`} id="agent-tokens-heading">
        Agent tokens
      </h2>
      <p className={`mt-1 text-sm ${textSecondaryOnCanvas}`}>
        Create a token for an agent you run. Its writes are attributed to you.
      </p>

      {tokens.isPending ? (
        <p className={`mt-6 ${textMutedOnCanvas}`}>Loading agent tokens…</p>
      ) : null}
      {tokens.isError ? (
        <div className="mt-6">
          <QueryError
            message="Couldn't load agent tokens."
            onRetry={() => void tokens.refetch()}
            retrying={tokens.isFetching}
          />
        </div>
      ) : null}
      {tokens.isSuccess ? (
        <div className={settingsTableWrapper}>
          <table className="w-full text-left text-sm">
            <thead className={settingsTableHead}>
              <tr>
                <th className="px-4 py-3 font-semibold" scope="col">
                  Name
                </th>
                <th className="px-4 py-3 font-semibold" scope="col">
                  Token
                </th>
                <th className="px-4 py-3 font-semibold" scope="col">
                  Created
                </th>
                <th className="px-4 py-3 font-semibold" scope="col">
                  Last used
                </th>
                <th className="px-4 py-3" scope="col">
                  <span className="sr-only">Actions</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {tokens.data.length === 0 ? (
                <tr>
                  <td className={`px-4 py-5 ${textMutedOnSurface}`} colSpan={5}>
                    No agent tokens yet.
                  </td>
                </tr>
              ) : (
                tokens.data.map((token) => (
                  <tr className={settingsTableRow} key={token.id}>
                    <td className={`px-4 py-3 font-medium ${textPrimaryOnSurface}`}>
                      {token.name}
                    </td>
                    <td className={`px-4 py-3 font-mono ${textSecondaryOnSurface}`}>
                      {`dsp_${token.prefix}…`}
                    </td>
                    <td className={`px-4 py-3 ${textSecondaryOnSurface}`}>
                      <Timestamp at={token.created_at} />
                    </td>
                    <td className={`px-4 py-3 ${textSecondaryOnSurface}`}>
                      {token.last_used_at === null ? (
                        "Never used"
                      ) : (
                        <Timestamp at={token.last_used_at} />
                      )}
                    </td>
                    <td className="px-4 py-3 text-right">
                      {token.revoked_at === null ? (
                        <button
                          aria-label={`Revoke ${token.name}`}
                          className={`min-h-11 px-2 font-medium disabled:cursor-not-allowed disabled:opacity-50 ${dangerText} ${dangerHoverText}`}
                          disabled={revokeToken.isPending}
                          onClick={() => revoke(token)}
                          type="button"
                        >
                          Revoke
                        </button>
                      ) : (
                        <span className={textMutedOnSurface}>Revoked</span>
                      )}
                    </td>
                  </tr>
                ))
              )}
            </tbody>
          </table>
        </div>
      ) : null}

      <form
        className={`mt-6 flex flex-col gap-3 rounded-xl border p-4 sm:flex-row sm:items-end ${borderDefault}`}
        onSubmit={submit}
      >
        <label
          className={`grid flex-1 gap-1 text-sm font-medium ${textSecondaryOnCanvas}`}
          htmlFor="agent-token-name"
        >
          Token label
          <input
            className={`min-h-11 rounded-md px-3 py-2 ${inputClasses(false)} ${textPrimaryOnSurface}`}
            id="agent-token-name"
            onChange={(event) => {
              setLostLabel(null);
              setName(event.target.value);
            }}
            placeholder="Architect"
            required
            value={name}
          />
        </label>
        <button className={settingsSubmitButton} disabled={createToken.isPending} type="submit">
          New token
        </button>
      </form>
      {lostLabel === null ? null : (
        <div className="mt-4 space-y-2">
          <LandedWithoutAnswer what={`Token “${lostLabel}”`}>
            Its value is shown only once and was lost with the answer, so revoke it and mint
            another.
          </LandedWithoutAnswer>
          <button
            className={settingsSubmitButton}
            disabled={lost === undefined || revokeToken.isPending || createToken.isPending}
            onClick={replaceLost}
            type="button"
          >
            {lost === undefined ? "Waiting for it to be listed…" : "Revoke it and mint another"}
          </button>
        </div>
      )}
      {createToken.isError ? (
        <div className="mt-4">
          <QueryError
            message="Couldn't create the agent token."
            onRetry={() => createToken.mutate({ name })}
            retrying={createToken.isPending}
          />
        </div>
      ) : null}
      {shownToken === undefined ? null : (
        <div className={`mt-4 rounded-xl border p-4 ${card}`} role="status">
          <p className={`font-medium ${textPrimaryOnSurface}`}>
            Copy this token now. It will not be shown again.
          </p>
          <div className="mt-3 flex flex-col gap-3 sm:flex-row sm:items-center">
            <code
              className={`break-all rounded-md px-3 py-2 select-text ${surfaceMutedBg} ${textPrimaryOnSurface}`}
            >
              {shownToken.token}
            </code>
            <button
              className={`min-h-11 rounded-md px-4 py-2 font-medium ${primaryButtonBg} ${primaryButtonHoverBg}`}
              onClick={copyToken}
              type="button"
            >
              Copy token
            </button>
          </div>
          {copyStatus === "idle" ? null : (
            <p
              aria-live="polite"
              className={`mt-2 text-sm ${copyStatus === "copied" ? successText : dangerText}`}
            >
              {copyStatus === "copied" ? "Copied" : "Copy failed - select the text"}
            </p>
          )}
          <p className={`mt-4 text-sm ${textSecondaryOnSurface}`}>
            Put this in <code>~/.config/legion/envoy.json</code>:
          </p>
          <pre
            className={`mt-2 overflow-x-auto rounded-md p-3 text-sm ${surfaceMutedBg} ${textPrimaryOnSurface}`}
          >
            <code>
              {JSON.stringify(
                {
                  dispatch: {
                    enabled: true,
                    serverUrl: window.location.origin,
                    token: shownToken.token,
                  },
                },
                null,
                2
              )}
            </code>
          </pre>
          <p className={`mt-2 text-sm ${textSecondaryOnSurface}`}>
            If the file exists, replace its <code>dispatch</code> section with this one and keep the
            rest. A session under a named omp profile, or with <code>CLAUDE_CONFIG_DIR</code> or{" "}
            <code>OPENCODE_CONFIG_DIR</code> set, reads the <code>envoy.json</code> in that
            directory instead; any other session reads the file <code>ENVOY_CONFIG_FILE</code> names
            when it is set.
          </p>
          <p className={`mt-4 text-sm ${textSecondaryOnSurface}`}>
            Or set these environment variables:
          </p>
          <pre
            className={`mt-2 overflow-x-auto rounded-md p-3 text-sm ${surfaceMutedBg} ${textPrimaryOnSurface}`}
          >
            <code>{`DISPATCH_URL=${window.location.origin}\nDISPATCH_TOKEN=${shownToken.token}`}</code>
          </pre>
        </div>
      )}
    </section>
  );
}
