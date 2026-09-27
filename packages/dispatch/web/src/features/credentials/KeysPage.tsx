import { useMutation, useQuery } from "@tanstack/react-query";
import { type ReactNode, useState } from "react";

import { api, apiErrorMessage, isCredentialFeatureOff } from "../../api/client";
import { whoAmIQuery } from "../../api/queries";
import type {
  PublicKeyCredentialCreationOptionsJSON,
  PublicKeyCredentialRequestOptionsJSON,
  RegistrationResponseJSON,
} from "../../api/types";
import { StatusPill } from "../../components/Pill";
import { QueryError } from "../../components/QueryError";
import { useCopyFeedback } from "../../hooks/useCopyFeedback";
import { useSubmitGuard } from "../../hooks/useSubmitGuard";
import { createCredential, getAssertion } from "../../lib/webauthn";
import {
  card,
  dangerText,
  inputClasses,
  primaryButtonBg,
  primaryButtonDisabled,
  primaryButtonEnabledHoverBg,
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
import { useDocumentTitle } from "../shell/useDocumentTitle";
import { GrantsSection } from "./GrantsSection";
import { credentialKeyHash, credentialKeysQuery } from "./keys";

/** The rules-file PR instruction below every YAML block this page hands the viewer. */
const RULES_FILE_INSTRUCTION =
  "Add this entry to meta/infra/config/agent-secret-rules.yaml in agent-c and open a PR.";

/** A YAML block from a register/endorse ceremony, with a copy button and the rules-file
 *  instruction — the same affordance `AgentTokensSection` uses for its config snippets. */
function YamlBlock({ heading, yaml }: { heading: string; yaml: string }): ReactNode {
  const { copy, status: copyStatus } = useCopyFeedback();
  return (
    <div className={`rounded-xl border p-4 ${card}`} role="status">
      <p className={`font-medium ${textPrimaryOnSurface}`}>{heading}</p>
      <pre
        className={`mt-2 overflow-x-auto rounded-md p-3 text-sm ${surfaceMutedBg} ${textPrimaryOnSurface}`}
      >
        <code>{yaml}</code>
      </pre>
      <div className="mt-2 flex items-center gap-3">
        <button
          className={`min-h-11 rounded-md px-4 py-2 text-sm font-medium ${primaryButtonBg} ${primaryButtonEnabledHoverBg}`}
          onClick={() => copy(yaml)}
          type="button"
        >
          Copy
        </button>
        {copyStatus === "idle" ? null : (
          <p
            aria-live="polite"
            className={`text-sm ${copyStatus === "copied" ? successText : dangerText}`}
          >
            {copyStatus === "copied" ? "Copied" : "Copy failed - select the text"}
          </p>
        )}
      </div>
      <p className={`mt-3 text-sm ${textSecondaryOnSurface}`}>{RULES_FILE_INSTRUCTION}</p>
    </div>
  );
}

/**
 * The approver's own key registration/list/endorse page (`/credentials/keys`), plus the live
 * grants list (`GrantsSection`). Design v4's "Approver keys": registering a key on this page
 * never persists it to the broker's key set by itself ("Nothing is persisted to the key set;
 * the key enters through the rules file only") — register returns a YAML fragment carrying the
 * new key's attested registration, and a second, already-live key must endorse that same
 * fragment (the endorse ceremony) before the pair is a complete rules-file entry ready for a
 * PR. So "Endorse" here operates on the just-registered key held in this page's own state, not
 * on a row from `GET .../keys` — a freshly registered, unendorsed key has no row there at all.
 */
export function KeysPage(): ReactNode {
  useDocumentTitle("Approver keys · Dispatch");
  const whoAmI = useQuery(whoAmIQuery());
  // `/auth/whoami` echoes GitHub's casing; the broker's key routes key on the lowercase login.
  const login = whoAmI.data?.login.toLowerCase();
  const keys = useQuery({ ...credentialKeysQuery(login ?? ""), enabled: login !== undefined });
  const registerSubmitGuard = useSubmitGuard();
  const endorseSubmitGuard = useSubmitGuard();

  const [selectedEndorserId, setSelectedEndorserId] = useState<string | undefined>(undefined);

  const activeKeys = keys.data?.keys.filter((key) => key.state === "active") ?? [];
  const selectedActiveKey = activeKeys.find((key) => key.credential_id === selectedEndorserId);
  const effectiveEndorserId =
    activeKeys.length === 1 ? activeKeys[0].credential_id : selectedActiveKey?.credential_id;

  const endorse = useMutation({
    mutationFn: async (newKey: RegistrationResponseJSON) => {
      if (login === undefined) {
        throw new Error("No signed-in login to endorse a key for");
      }
      // The signer is an already-live key the user chose (or the sole active key, auto-picked
      // when there is only one) — never the just-registered `newKey` itself: a freshly
      // registered key has no persisted `approver_keys` row yet, so the broker can never find
      // it as an endorser (see the doc comment on `BeginEndorse`, approvers.go).
      if (effectiveEndorserId === undefined) {
        throw new Error("No active key to endorse with");
      }
      const keyHash = await credentialKeyHash(newKey.id);
      const begin = await api.beginCredentialKeyEndorsement(login, {
        credential_id: effectiveEndorserId,
        key_hash: keyHash,
      });
      // contract v9's endorse/begin route names the whole PublicKeyCredentialRequestOptionsJSON
      // (unlike approve/deny's bare-string challenges): registration requests non-resident keys
      // (residentKey: "discouraged"), so allowCredentials is what lets a real hardware
      // authenticator locate the already-live key that must sign this endorsement.
      const requestOptions = begin.publicKey as PublicKeyCredentialRequestOptionsJSON;
      const assertion = await getAssertion(
        requestOptions.challenge,
        window.location.hostname,
        requestOptions.allowCredentials
      );
      return api.finishCredentialKeyEndorsement(login, {
        ceremony_id: begin.ceremony_id,
        response: assertion,
      });
    },
    onSettled: () => endorseSubmitGuard.release(),
  });
  const register = useMutation({
    mutationFn: async () => {
      if (login === undefined) {
        throw new Error("No signed-in login to register a key for");
      }
      const begin = await api.beginCredentialKeyRegistration(login);
      const response = await createCredential(
        begin.publicKey as PublicKeyCredentialCreationOptionsJSON
      );
      const finish = await api.finishCredentialKeyRegistration(login, {
        ceremony_id: begin.ceremony_id,
        response,
      });
      return { finish, response };
    },
    // A second register->endorse cycle in one page visit is design v4's routine key-rotation
    // flow ("endorse-new, then remove-old"), not misuse — a fresh registration must clear the
    // prior cycle's stale endorsement so the new key's YAML doesn't sit next to an old
    // endorsement block, and the Endorse button reappears for the new key.
    onSuccess: () => {
      endorse.reset();
      setSelectedEndorserId(undefined);
    },
    onSettled: () => registerSubmitGuard.release(),
  });

  const newKey = register.data?.response;

  if (keys.isError && isCredentialFeatureOff(keys.error)) {
    return (
      <section className="max-w-2xl space-y-6">
        <header>
          <h1 className={`text-xl font-semibold ${textPrimaryOnCanvas}`}>Approver keys</h1>
          <p className={`mt-1 text-sm ${textSecondaryOnCanvas}`}>
            The WebAuthn keys the broker trusts you to approve credential requests with.
          </p>
        </header>
        <QueryError message="Credential requests are not configured on this Dispatch deployment." />
      </section>
    );
  }

  return (
    <section className="max-w-2xl space-y-6">
      <header>
        <h1 className={`text-xl font-semibold ${textPrimaryOnCanvas}`}>Approver keys</h1>
        <p className={`mt-1 text-sm ${textSecondaryOnCanvas}`}>
          The WebAuthn keys the broker trusts you to approve credential requests with.
        </p>
      </header>

      {whoAmI.isPending || keys.isPending ? (
        <p className={textMutedOnCanvas}>Loading keys…</p>
      ) : null}
      {keys.isError ? (
        <QueryError
          message="Couldn't load your keys."
          onRetry={() => void keys.refetch()}
          retrying={keys.isFetching}
        />
      ) : null}
      {keys.isSuccess ? (
        <div className={`overflow-x-auto rounded-xl border ${card}`}>
          <table className="w-full text-left text-sm">
            <thead className={`border-b ${textSecondaryOnSurface}`}>
              <tr>
                <th className="px-4 py-3 font-semibold" scope="col">
                  AAGUID
                </th>
                <th className="px-4 py-3 font-semibold" scope="col">
                  Registered
                </th>
                <th className="px-4 py-3 font-semibold" scope="col">
                  Last used
                </th>
                <th className="px-4 py-3 font-semibold" scope="col">
                  State
                </th>
              </tr>
            </thead>
            <tbody>
              {keys.data.keys.length === 0 ? (
                <tr>
                  <td className={`px-4 py-5 ${textMutedOnSurface}`} colSpan={4}>
                    No keys registered yet.
                  </td>
                </tr>
              ) : (
                keys.data.keys.map((key) => (
                  <tr className="border-b last:border-0" key={key.credential_id}>
                    <td className={`px-4 py-3 font-mono ${textPrimaryOnSurface}`}>{key.aaguid}</td>
                    <td className={`px-4 py-3 ${textSecondaryOnSurface}`}>
                      <Timestamp at={key.registered_at} />
                    </td>
                    <td className={`px-4 py-3 ${textSecondaryOnSurface}`}>
                      {key.last_used_at === null ? (
                        "Never used"
                      ) : (
                        <Timestamp at={key.last_used_at} />
                      )}
                    </td>
                    <td className="px-4 py-3">
                      <StatusPill>
                        {key.state.charAt(0).toUpperCase() + key.state.slice(1)}
                      </StatusPill>
                    </td>
                  </tr>
                ))
              )}
            </tbody>
          </table>
        </div>
      ) : null}

      <div className="space-y-4">
        <button
          className={`min-h-11 rounded-lg px-4 py-2 text-sm font-medium ${primaryButtonBg} ${primaryButtonEnabledHoverBg} ${primaryButtonDisabled}`}
          disabled={register.isPending || login === undefined}
          onClick={() => registerSubmitGuard.guard(() => register.mutate())}
          type="button"
        >
          Register key
        </button>
        {register.isError ? (
          <p className={dangerText}>
            {apiErrorMessage(register.error, "Could not register this key.")}
          </p>
        ) : null}
        {register.data === undefined ? null : (
          <>
            <YamlBlock heading="New key registered" yaml={register.data.finish.yaml} />
            {endorse.data === undefined ? (
              <div className="space-y-2">
                <p className={`text-sm ${textSecondaryOnCanvas}`}>
                  This key isn't live yet — an already-active key must endorse it before you add it
                  to the rules file.
                </p>
                {activeKeys.length > 1 ? (
                  <div className="space-y-1">
                    <label
                      className={`block text-sm font-medium ${textSecondaryOnCanvas}`}
                      htmlFor="endorser-key"
                    >
                      Choose which key will endorse it
                    </label>
                    <select
                      className={`rounded-md px-3 py-2 ${inputClasses(false)} ${textPrimaryOnSurface}`}
                      id="endorser-key"
                      onChange={(event) => setSelectedEndorserId(event.target.value)}
                      required
                      value={selectedActiveKey?.credential_id ?? ""}
                    >
                      <option disabled value="">
                        Choose a key
                      </option>
                      {activeKeys.map((key) => (
                        <option key={key.credential_id} value={key.credential_id}>
                          {key.aaguid} · registered {new Date(key.registered_at).toLocaleString()} ·{" "}
                          {key.last_used_at === null
                            ? "never used"
                            : `last used ${new Date(key.last_used_at).toLocaleString()}`}
                        </option>
                      ))}
                    </select>
                  </div>
                ) : null}
                <button
                  className={`min-h-11 rounded-lg px-4 py-2 text-sm font-medium ${primaryButtonBg} ${primaryButtonEnabledHoverBg} ${primaryButtonDisabled}`}
                  disabled={
                    endorse.isPending || newKey === undefined || effectiveEndorserId === undefined
                  }
                  onClick={() =>
                    newKey !== undefined && endorseSubmitGuard.guard(() => endorse.mutate(newKey))
                  }
                  type="button"
                >
                  Endorse with another key
                </button>
                {endorse.isError ? (
                  <p className={dangerText}>
                    {apiErrorMessage(endorse.error, "Could not endorse this key.")}
                  </p>
                ) : null}
              </div>
            ) : (
              <YamlBlock heading="Endorsement" yaml={endorse.data.yaml} />
            )}
          </>
        )}
      </div>

      <GrantsSection />
    </section>
  );
}
