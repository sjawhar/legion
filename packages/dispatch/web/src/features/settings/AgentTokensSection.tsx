import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { type FormEvent, type ReactNode, useState } from "react";

import { api } from "../../api/client";
import type { AgentToken, CreatedAgentToken } from "../../api/types";
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
import { Timestamp } from "../refs/Timestamp";

const agentTokensQueryKey = ["agent-tokens"] as const;

export function AgentTokensSection(): ReactNode {
  const queryClient = useQueryClient();
  const [name, setName] = useState("");
  const [shownToken, setShownToken] = useState<CreatedAgentToken>();
  const [copyError, setCopyError] = useState<string>();
  const tokens = useQuery({ queryKey: agentTokensQueryKey, queryFn: () => api.listAgentTokens() });
  const createToken = useMutation({
    mutationFn: (input: { name: string }) => api.createAgentToken(input),
    onSuccess: (created) => {
      const { token, ...safeToken } = created;
      queryClient.setQueryData<AgentToken[]>(agentTokensQueryKey, (current = []) => [
        safeToken,
        ...current,
      ]);
      setShownToken(created);
      setCopyError(undefined);
      setName("");
    },
  });
  const revokeToken = useMutation({
    mutationFn: (id: string) => api.revokeAgentToken(id),
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
    createToken.mutate({ name });
  };
  const copyToken = () => {
    if (shownToken === undefined) {
      return;
    }
    void navigator.clipboard.writeText(shownToken.token).then(
      () => setCopyError(undefined),
      () => setCopyError("Couldn't copy the token. Copy it from the field above.")
    );
  };
  const revoke = (token: AgentToken) => {
    if (
      window.confirm(`Revoke ${token.name}? Agents using this token will stop working immediately.`)
    ) {
      revokeToken.mutate(token.id);
    }
  };

  return (
    <section aria-labelledby="agent-tokens-heading" className="mt-10">
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
        <div className={`mt-6 overflow-x-auto rounded-xl border ${card}`}>
          <table className="w-full text-left text-sm">
            <thead
              className={`border-b ${surfaceMutedBg} ${borderDefault} ${textSecondaryOnSurface}`}
            >
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
                  <tr className={`border-b last:border-0 ${borderDefault}`} key={token.id}>
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
            onChange={(event) => setName(event.target.value)}
            placeholder="Architect"
            required
            value={name}
          />
        </label>
        <button
          className={`min-h-11 rounded-md px-4 py-2 font-medium disabled:cursor-not-allowed disabled:opacity-50 ${primaryButtonBg} ${primaryButtonHoverBg}`}
          disabled={createToken.isPending}
          type="submit"
        >
          New token
        </button>
      </form>
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
              className={`break-all rounded-md px-3 py-2 ${surfaceMutedBg} ${textPrimaryOnSurface}`}
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
          {copyError === undefined ? null : (
            <p className={`mt-2 text-sm ${dangerText}`}>{copyError}</p>
          )}
          <p className={`mt-4 text-sm ${textSecondaryOnSurface}`}>
            Put this in <code>~/.config/opencode/envoy.json</code>:
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
            This merges into your existing envoy.json file.
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
