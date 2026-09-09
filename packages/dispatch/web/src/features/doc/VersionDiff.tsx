import { diffLines } from "diff";
import type { ReactNode } from "react";

interface VersionDiffProps {
  before: string;
  after: string;
}

export function VersionDiff({ before, after }: VersionDiffProps): ReactNode {
  return (
    <pre
      className="overflow-x-auto rounded-lg border border-slate-200 bg-white p-4 text-sm leading-6"
      data-testid="version-diff"
    >
      <code>
        {diffLines(before, after).map((part) => {
          if (part.added) {
            return (
              <ins
                className="added block bg-emerald-100 text-emerald-950 no-underline"
                key={`added:${part.value}`}
              >
                {part.value}
              </ins>
            );
          }
          if (part.removed) {
            return (
              <del
                className="removed block bg-rose-100 text-rose-950 no-underline"
                key={`removed:${part.value}`}
              >
                {part.value}
              </del>
            );
          }
          return (
            <span className="block" key={`unchanged:${part.value}`}>
              {part.value}
            </span>
          );
        })}
      </code>
    </pre>
  );
}
