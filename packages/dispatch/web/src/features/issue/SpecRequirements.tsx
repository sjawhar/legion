import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";

import { issueCoverageQuery } from "../../api/queries";
import type { RequirementCoverage } from "../../api/types";
import { EmptyState } from "../../components/EmptyState";
import { pillShapeClassName } from "../../components/Pill";
import {
  card,
  dangerText,
  inlineWarningText,
  surfaceMutedBg,
  textMutedOnCanvas,
  textMutedOnSurface,
  textPrimaryOnSurface,
} from "../../theme/classes";
import { BlockReferences } from "../refs/ReferencedBy";

function Requirement({ requirement }: { requirement: RequirementCoverage }): ReactNode {
  // What the server says this requirement lacks, in the server's own words. The server decides
  // which lacks count - a requirement with no plan task is no gap until the issue has a plan
  // document - so nothing is judged again here.
  const gaps = requirement.gaps ?? [];
  return (
    <li
      className={`space-y-2 rounded-lg p-3 ${card}`}
      data-covered={gaps.length === 0}
      data-testid="spec-requirement"
    >
      <div className="flex flex-wrap items-baseline gap-x-2 gap-y-1">
        <span className={textPrimaryOnSurface}>{requirement.text}</span>
        {gaps.map((gap) => (
          <span
            className={`${pillShapeClassName} ${surfaceMutedBg} ${inlineWarningText}`}
            data-testid="spec-requirement-gap"
            key={gap}
          >
            {gap}
          </span>
        ))}
      </div>
      <BlockReferences reference={requirement.ref} />
    </li>
  );
}

/**
 * The requirements of the issue's spec, each flagged with what it lacks
 * (`GET /api/v1/issues/{key}/coverage`). It sits under the spec on the Spec tab, where the
 * document itself draws the requirement's words: here they carry the flag and the Referenced-by
 * list, which the document's own drawing has nowhere to put.
 */
export function SpecRequirements({ issueKey }: { issueKey: string }): ReactNode {
  const coverage = useQuery(issueCoverageQuery(issueKey));

  if (coverage.isPending) {
    return (
      <p className={textMutedOnCanvas} data-testid="spec-requirements-loading">
        Loading requirements…
      </p>
    );
  }
  if (coverage.isError) {
    return (
      <p className={dangerText} data-testid="spec-requirements-error">
        Could not load the requirement coverage.
      </p>
    );
  }
  const requirements = coverage.data.requirements ?? [];
  if (requirements.length === 0) {
    return (
      <div data-testid="spec-no-requirements">
        <EmptyState label="Requirements empty state" message="This spec marks no requirements." />
      </div>
    );
  }

  return (
    <section className="space-y-2" data-testid="spec-requirements">
      <h2 className={`text-sm font-semibold ${textMutedOnSurface}`}>Requirements</h2>
      <ul aria-label="Requirements" className="space-y-3">
        {requirements.map((requirement) => (
          <Requirement key={requirement.block_id} requirement={requirement} />
        ))}
      </ul>
    </section>
  );
}
