import { diffLines } from "diff";
import type { ReactNode } from "react";

import {
  card,
  diffAddedBg,
  diffAddedText,
  diffRemovedBg,
  diffRemovedText,
} from "../../theme/classes";

interface VersionDiffProps {
  before: string;
  after: string;
}

export function VersionDiff({ before, after }: VersionDiffProps): ReactNode {
  return (
    <pre
      className={`[overflow-wrap:anywhere] rounded-lg p-4 text-sm leading-6 whitespace-pre-wrap ${card}`}
      data-testid="version-diff"
    >
      <code>
        {diffLines(before, after).map((part) => {
          if (part.added) {
            return (
              <ins
                className={`added block no-underline ${diffAddedBg} ${diffAddedText}`}
                key={`added:${part.value}`}
              >
                {part.value}
              </ins>
            );
          }
          if (part.removed) {
            return (
              <del
                className={`removed block no-underline ${diffRemovedBg} ${diffRemovedText}`}
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
