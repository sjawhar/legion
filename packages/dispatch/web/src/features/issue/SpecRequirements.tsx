import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";

import { issueCoverageQuery } from "../../api/queries";
import type { IssueCoverage, RequirementCoverage } from "../../api/types";
import { EmptyState } from "../../components/EmptyState";
import { pillClassName } from "../../components/Pill";
import {
  card,
  dangerText,
  inlineWarningText,
  textMutedOnCanvas,
  textMutedOnSurface,
  textPrimaryOnSurface,
} from "../../theme/classes";
import { BlockReferences } from "../refs/ReferencedBy";

/**
 * What the server says this requirement lacks, as it wrote it: its lines of `IssueCoverage.gaps`
 * with the block id they name stripped, so `b3 has no check` reads `has no check`. The server
 * decides which lacks count - a requirement with no plan task is no gap until the issue has a
 * plan document - so nothing is judged again here.
 */
export function requirementGaps(
  coverage: Pick<IssueCoverage, "gaps">,
  requirement: Pick<RequirementCoverage, "block_id">
): string[] {
  const prefix = `${requirement.block_id} `;
  return coverage.gaps
    .filter((gap) => gap.startsWith(prefix))
    .map((gap) => gap.slice(prefix.length));
}

function Requirement({
  coverage,
  requirement,
}: {
  coverage: IssueCoverage;
  requirement: RequirementCoverage;
}): ReactNode {
  const gaps = requirementGaps(coverage, requirement);
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
            className={`${pillClassName("label")} ${inlineWarningText}`}
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
  if (coverage.data.requirements.length === 0) {
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
        {coverage.data.requirements.map((requirement) => (
          <Requirement
            coverage={coverage.data}
            key={requirement.block_id}
            requirement={requirement}
          />
        ))}
      </ul>
    </section>
  );
}
