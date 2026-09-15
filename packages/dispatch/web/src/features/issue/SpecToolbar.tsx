import { type ReactNode, useState } from "react";

import {
  dangerText,
  inputClasses,
  secondaryButtonBorder,
  secondaryButtonDisabledText,
  secondaryButtonHoverBorder,
  secondaryButtonText,
  successText,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { ConnectionDot } from "../doc/ConnectionDot";
import type { DocumentToolbar } from "../doc/ProofDocument";
import { CopyRefButton } from "../refs/CopyRefButton";
import type { DispatchReferenceRoute } from "../refs/routes";

/** Compact primary-Spec controls displayed at the end of the active issue tab row. `reference`
 *  is the document as shown — the spec, or a historical version of it. */
export function SpecToolbar({
  isClosed,
  onShowDiffChange,
  onVersionChange,
  reference,
  showDiff,
  toolbar,
  version,
}: {
  isClosed: boolean;
  onShowDiffChange(next: boolean): void;
  onVersionChange(version: number | null): void;
  reference: DispatchReferenceRoute;
  showDiff: boolean;
  toolbar: DocumentToolbar;
  version: number | undefined;
}): ReactNode {
  const [copyFeedback, setCopyFeedback] = useState<"copied" | "failed" | undefined>(undefined);
  return (
    <div className="flex w-full min-w-0 flex-wrap items-center gap-2 md:ml-auto md:w-auto">
      <label
        className={`flex min-h-11 min-w-0 items-center gap-2 text-sm font-medium md:min-h-8 ${textSecondaryOnCanvas}`}
      >
        Version
        <select
          aria-label="Version"
          className={`min-h-11 min-w-0 max-w-56 truncate rounded border px-2 py-2 font-normal md:min-h-8 md:py-1 ${inputClasses(false)}`}
          onChange={(event) =>
            onVersionChange(event.target.value === "" ? null : Number(event.target.value))
          }
          value={version ?? ""}
        >
          <option value="">Current</option>
          {toolbar.versions.map((item) => (
            <option key={item.number} value={item.number}>
              Version {item.number}
              {item.named && item.summary !== null ? ` — ${item.summary}` : ""}
            </option>
          ))}
        </select>
      </label>
      <button
        aria-label="Name version"
        className={`min-h-11 shrink-0 rounded-lg px-3 py-2 text-sm font-medium md:min-h-8 md:py-1 ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder} ${secondaryButtonDisabledText}`}
        disabled={isClosed || toolbar.isNamingVersion}
        onClick={toolbar.requestNamedVersion}
        type="button"
      >
        Name
      </button>
      <ConnectionDot connection={toolbar.connection} />
      <CopyRefButton route={reference} />
      {version === undefined ? (
        <>
          <button
            aria-label="Copy link to block"
            className={`min-h-11 shrink-0 rounded-lg px-3 py-2 text-sm font-medium md:min-h-8 md:py-1 ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder} ${secondaryButtonDisabledText}`}
            onClick={() => {
              void toolbar.copyBlockLink().then(
                (copied) => setCopyFeedback(copied ? "copied" : "failed"),
                () => setCopyFeedback("failed")
              );
            }}
            type="button"
          >
            Copy link
          </button>
          {copyFeedback === undefined ? null : (
            <span
              className={`text-xs font-medium ${copyFeedback === "copied" ? successText : dangerText}`}
              role="status"
            >
              {copyFeedback === "copied" ? "Copied" : "Copy failed - select the text"}
            </span>
          )}
        </>
      ) : (
        <button
          className={`min-h-11 shrink-0 rounded-lg px-3 py-2 text-sm font-medium md:min-h-8 md:py-1 ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder}`}
          onClick={() => onShowDiffChange(!showDiff)}
          type="button"
        >
          {showDiff ? "Show version" : "Diff vs current"}
        </button>
      )}
    </div>
  );
}
