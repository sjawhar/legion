import { useQuery, useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useState } from "react";

import { api, apiErrorMessage, type CreateArtifactGrantInput } from "../../api/client";
import type { Artifact, ArtifactGrant } from "../../api/types";
import { QueryError } from "../../components/QueryError";
import { useIsHuman } from "../../hooks/useIsHuman";
import { useSubmitGuard } from "../../hooks/useSubmitGuard";
import {
  borderDefault,
  dangerText,
  hoverToDangerText,
  inputClasses,
  primaryButtonBg,
  primaryButtonHoverBg,
  secondaryButtonBorder,
  secondaryButtonDisabledText,
  secondaryButtonHoverBorder,
  secondaryButtonText,
  textMutedOnSurface,
  textPrimaryOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { Timestamp } from "../refs/Timestamp";
import { LandedWithoutAnswer } from "../writes/LandedWithoutAnswer";
import { useKeyedWrite } from "../writes/useKeyedWrite";
import { versionsNewestFirst } from "./ArtifactHeader";

/**
 * Whether this file is a page file a `view` block can show, read as the server reads it
 * (`isPageFile` in packages/envoy/internal/dispatch/api/artifacts.go): a file whose stored type
 * is `text/html`, or whose name ends `.html` or `.htm`. The type is the one the version on
 * screen was stored under - the version the route names, else the newest, as `ArtifactBlobView`
 * picks the version whose size it shows. A document is never a page file, because its text is
 * markdown Dispatch rewrites rather than the bytes an author sent, and nor is a file an agent's
 * conversation owns, which belongs to neither an issue nor a project and whose uploads announce
 * nothing a page could redraw from. The grants routes refuse every other file `415 NOT_A_PAGE`,
 * so the panel asks for no list it could not get.
 */
function isPageFile(artifact: Artifact, version: number | undefined): boolean {
  if (artifact.kind === "doc" || (artifact.issue_key === null && artifact.project === "")) {
    return false;
  }
  const versions = versionsNewestFirst(artifact.versions);
  const selected =
    version === undefined ? versions[0] : versions.find((item) => item.number === version);
  const mime = selected?.mime?.split(";")[0]?.trim().toLowerCase();
  if (mime === "text/html") {
    return true;
  }
  const name = artifact.name.toLowerCase();
  return name.endsWith(".html") || name.endsWith(".htm");
}

/**
 * "Data this page may read": the data files a person has granted this page file
 * (`dispatch://INFRA-682`, "Data grants"). A `view` block names a page and the data files it
 * draws, but any author writes a block, an agent included, so the block never decides what the
 * page is given: these grants do, and only a person writes one. The panel is therefore shown to
 * a person alone (`useIsHuman`), on an issue's or a project's page file alone, and it is where
 * the block's "not granted to this page" line sends a reader.
 *
 * Each row names the data file, the version where the grant pins one, who granted it and when.
 * A refusal is shown as the server wrote it, so `DATA_FILE_IS_A_DOCUMENT` and the rest read as
 * the sentence that says what to correct rather than as a status.
 */
export function ArtifactGrantsPanel({
  artifact,
  version,
}: {
  artifact: Artifact;
  version: number | undefined;
}): ReactNode {
  const queryClient = useQueryClient();
  const shown = useIsHuman() && isPageFile(artifact, version);
  const [reference, setReference] = useState("");
  const addGuard = useSubmitGuard();
  const grants = useQuery({
    enabled: shown,
    queryKey: ["artifact-grants", artifact.id],
    queryFn: () => api.listArtifactGrants(artifact.id),
  });
  const reread = () =>
    void queryClient.invalidateQueries({ queryKey: ["artifact-grants", artifact.id] });
  // Both writes show their own refusal here, under the list the reader is looking at.
  const grant = useKeyedWrite({
    inline: true,
    label: (input: CreateArtifactGrantInput) => `${artifact.name} may read ${input.ref}`,
    onSettled: () => addGuard.release(),
    onSuccess: () => {
      setReference("");
      reread();
    },
    write: (input: CreateArtifactGrantInput, bounds) =>
      api.createArtifactGrant(artifact.id, input, bounds),
  });
  const revoke = useKeyedWrite({
    inline: true,
    label: (row: ArtifactGrant) => `${artifact.name} may no longer read ${row.name}`,
    onSuccess: reread,
    write: (row: ArtifactGrant, bounds) => api.deleteArtifactGrant(artifact.id, row.id, bounds),
  });

  if (!shown) {
    return null;
  }

  return (
    <section
      aria-labelledby="artifact-grants-heading"
      className={`rounded-xl border p-4 ${borderDefault}`}
    >
      <h2 className={`text-sm font-semibold ${textPrimaryOnSurface}`} id="artifact-grants-heading">
        Data this page may read
      </h2>
      <p className={`mt-1 text-sm ${textSecondaryOnSurface}`}>
        A <code>view</code> block of this page gets these files and no others. Only a person adds or
        removes one.
      </p>

      {grants.isPending ? (
        <p className={`mt-3 text-sm ${textMutedOnSurface}`}>Loading the granted data files…</p>
      ) : null}
      {grants.isError ? (
        <div className="mt-3">
          <QueryError
            message={apiErrorMessage(grants.error, "Could not load the granted data files.")}
            onRetry={() => void grants.refetch()}
            retrying={grants.isFetching}
          />
        </div>
      ) : null}
      {grants.isSuccess ? (
        grants.data.length === 0 ? (
          <p className={`mt-3 text-sm ${textMutedOnSurface}`}>
            No data file is granted to this page.
          </p>
        ) : (
          <ul className="mt-3 space-y-2">
            {grants.data.map((row) => (
              <li
                className={`flex flex-wrap items-baseline gap-x-3 gap-y-1 rounded-lg border p-3 text-sm ${borderDefault}`}
                key={row.id}
              >
                <span className={`font-medium ${textPrimaryOnSurface}`}>{row.name}</span>
                <code className={`text-xs ${textSecondaryOnSurface}`}>{row.ref}</code>
                <span className={textSecondaryOnSurface}>
                  {row.version === null ? "Every version" : `Version ${row.version}`}
                </span>
                <span className={textSecondaryOnSurface}>
                  Granted by {row.granted_by} <Timestamp at={row.created_at} />
                </span>
                <button
                  aria-label={`Remove the grant for ${row.name}`}
                  className={`ml-auto min-h-11 rounded-lg border px-3 py-1 text-xs font-medium whitespace-nowrap disabled:cursor-not-allowed disabled:opacity-50 ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder} ${secondaryButtonDisabledText} ${hoverToDangerText}`}
                  disabled={revoke.isPending}
                  onClick={() => revoke.mutate(row)}
                  type="button"
                >
                  Remove
                </button>
              </li>
            ))}
          </ul>
        )
      ) : null}

      <form
        className="mt-3 grid gap-2 sm:grid-cols-[1fr_auto] sm:items-end"
        onSubmit={(event) => {
          event.preventDefault();
          const ref = reference.trim();
          if (ref === "") {
            return;
          }
          addGuard.guard(() => grant.mutate({ ref }));
        }}
      >
        <label
          className={`grid gap-1 text-sm font-medium ${textSecondaryOnSurface}`}
          htmlFor="artifact-grant-reference"
        >
          Data file
          <input
            className={`min-h-11 rounded-md px-3 py-2 font-mono ${inputClasses(false)} ${textPrimaryOnSurface}`}
            disabled={grant.isPending}
            id="artifact-grant-reference"
            onChange={(event) => {
              grant.forgetUnanswered();
              setReference(event.target.value);
            }}
            placeholder="dispatch://KEY/artifact/kernel-loop-json"
            required
            value={reference}
          />
        </label>
        <button
          className={`min-h-11 rounded-md px-4 py-2 font-medium disabled:cursor-not-allowed disabled:opacity-50 ${primaryButtonBg} ${primaryButtonHoverBg}`}
          disabled={grant.isPending}
          type="submit"
        >
          Grant
        </button>
      </form>
      {grant.unanswered === undefined ? null : (
        <div className="mt-2">
          <LandedWithoutAnswer what={`The grant for ${grant.unanswered.variables.ref}`} />
        </div>
      )}
      {grant.isError ? (
        <p className={`mt-2 text-sm ${dangerText}`}>
          {apiErrorMessage(grant.error, "Could not grant that data file.")}
        </p>
      ) : null}
      {revoke.isError ? (
        <p className={`mt-2 text-sm ${dangerText}`}>
          {apiErrorMessage(revoke.error, "Could not remove that grant.")}
        </p>
      ) : null}
    </section>
  );
}
