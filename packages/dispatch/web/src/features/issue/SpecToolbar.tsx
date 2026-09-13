import type { ReactNode } from "react";

import {
  inputClasses,
  secondaryButtonBorder,
  secondaryButtonDisabledText,
  secondaryButtonHoverBorder,
  secondaryButtonText,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { ConnectionDot } from "../doc/ConnectionDot";
import type { DocumentToolbar } from "../doc/ProofDocument";

/** Version controls belong to the issue's Spec panel, where they operate on its primary document. */
export function SpecToolbar({
  isClosed,
  onShowDiffChange,
  onVersionChange,
  showDiff,
  toolbar,
  version,
}: {
  isClosed: boolean;
  onShowDiffChange(next: boolean): void;
  onVersionChange(version: number | null): void;
  showDiff: boolean;
  toolbar: DocumentToolbar;
  version: number | undefined;
}): ReactNode {
  return (
    <div className="flex min-w-0 flex-wrap items-center gap-2">
      <label
        className={`flex min-h-11 min-w-0 items-center gap-2 text-sm font-medium ${textSecondaryOnCanvas}`}
      >
        Version
        <select
          aria-label="Version"
          className={`min-h-11 min-w-0 max-w-56 truncate rounded border px-2 py-2 font-normal ${inputClasses(false)}`}
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
        className={`min-h-11 shrink-0 rounded-lg px-3 py-2 text-sm font-medium ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder} ${secondaryButtonDisabledText}`}
        disabled={isClosed || toolbar.isNamingVersion}
        onClick={toolbar.requestNamedVersion}
        type="button"
      >
        Name version
      </button>
      {version === undefined ? null : (
        <button
          className={`min-h-11 shrink-0 rounded-lg px-3 py-2 text-sm font-medium ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder}`}
          onClick={() => onShowDiffChange(!showDiff)}
          type="button"
        >
          {showDiff ? "Show version" : "Diff vs current"}
        </button>
      )}
      <ConnectionDot connection={toolbar.connection} />
    </div>
  );
}
