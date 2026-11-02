import { parseBlockPointerValue } from "@legion/contracts";
import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import { api } from "../../api/client";
import type { Artifact, ArtifactBlock } from "../../api/types";
import { EmptyState } from "../../components/EmptyState";
import {
  card,
  dangerText,
  linkHoverText,
  linkText,
  textMutedOnCanvas,
  textMutedOnSurface,
} from "../../theme/classes";
import { ApprovalChip } from "../doc/ApprovalChip";
import { withBlock } from "../refs/block-reference";
import { BlockReferences } from "../refs/ReferencedBy";
import { referenceTriggerProps } from "../refs/RefPreview";
import { useReferenceTarget } from "../refs/reference-target";
import {
  buildDispatchReference,
  buildReferencePath,
  documentRoute,
  parseNodeReference,
  shortForm,
} from "../refs/routes";

/** The `block[]` attribute `name` of `block` as the pointers it holds; empty for a block that
 *  carries none and for a value the server would refuse, which no stored block holds. */
function pointers(block: ArtifactBlock, name: string): readonly string[] {
  const value = block.attributes?.find((attribute) => attribute.name === name)?.value;
  return value === undefined ? [] : (parseBlockPointerValue(value) ?? []);
}

/** A block a pointer names, drawn as its own words once the graph answers them and as the
 *  reference's short form until then; a pointer that names nothing shows as the text it is. */
function BlockLink({ reference }: { reference: string }): ReactNode {
  const route = parseNodeReference(reference);
  const { title } = useReferenceTarget(route, { text: false });
  if (route === undefined) {
    return <span>{reference}</span>;
  }
  return (
    <Link
      {...referenceTriggerProps(route)}
      className={`underline ${linkText} ${linkHoverText}`}
      to={buildReferencePath(route)}
    >
      {title ?? shortForm(route)}
    </Link>
  );
}

/** One side of a task's pointers: the requirements it serves, or the tasks it needs. */
function Pointers({
  label,
  references,
  testId,
}: {
  label: string;
  references: readonly string[];
  testId: string;
}): ReactNode {
  return (
    <div className="flex flex-wrap items-baseline gap-x-2 gap-y-1" data-testid={testId}>
      <span className={`text-xs font-medium ${textMutedOnSurface}`}>{label}</span>
      {references.length === 0 ? (
        <span className={`text-sm ${textMutedOnSurface}`}>none</span>
      ) : (
        references.map((reference) => <BlockLink key={reference} reference={reference} />)
      )}
    </div>
  );
}

function Task({ block, reference }: { block: ArtifactBlock; reference: string }): ReactNode {
  return (
    <li className={`space-y-2 rounded-lg p-3 ${card}`} data-testid="plan-task">
      <BlockLink reference={reference} />
      <Pointers label="Serves" references={pointers(block, "serves")} testId="plan-task-serves" />
      <Pointers label="Needs" references={pointers(block, "needs")} testId="plan-task-needs" />
      <BlockReferences reference={reference} />
    </li>
  );
}

function Tasks({ artifact }: { artifact: Artifact }): ReactNode {
  // The same key the document's own block read uses, so a tab opened after the document is
  // already answered.
  const blocks = useQuery({
    queryFn: () => api.getArtifactBlocks(artifact.id),
    queryKey: ["artifact", artifact.id, "blocks"],
  });

  if (blocks.isPending) {
    return (
      <p className={textMutedOnCanvas} data-testid="plan-loading">
        Loading tasks…
      </p>
    );
  }
  if (blocks.isError) {
    return (
      <p className={dangerText} data-testid="plan-error">
        Could not load the plan's tasks.
      </p>
    );
  }

  const document = documentRoute(artifact);
  const tasks = blocks.data
    .filter((block) => block.type === "plan_task")
    .map((block) => ({ block, route: withBlock(document, block.id) }))
    .flatMap(({ block, route }) =>
      route === undefined ? [] : [{ block, reference: buildDispatchReference(route) }]
    );

  if (tasks.length === 0) {
    return (
      <div data-testid="plan-no-tasks">
        <EmptyState label="Plan tasks empty state" message="This plan marks no tasks." />
      </div>
    );
  }

  return (
    <ul aria-label="Plan tasks" className="space-y-3" data-testid="plan-tasks">
      {tasks.map(({ block, reference }) => (
        <Task block={block} key={block.id} reference={reference} />
      ))}
    </ul>
  );
}

/**
 * The issue's plan document whole - `children`, which the page renders - with the plan review's
 * approval beside it and, under it, each `plan_task` of the plan with the requirements it serves
 * and the tasks it needs. An issue with no plan document shows the command that makes one.
 */
export function PlanTab({
  artifact,
  children,
  issueKey,
}: {
  artifact: Artifact | undefined;
  children?: ReactNode;
  issueKey: string;
}): ReactNode {
  if (artifact === undefined) {
    return (
      <div data-testid="plan-empty">
        <EmptyState
          action={<code>{`dispatch artifact --issue ${issueKey} --name plan.md`}</code>}
          label="Plan empty state"
          message={`${issueKey} has no plan document. Make one with:`}
        />
      </div>
    );
  }

  return (
    <section className="space-y-4" data-testid="plan-tab">
      <div className="flex flex-wrap items-center gap-2" data-testid="plan-approval">
        <ApprovalChip artifact={artifact} />
      </div>
      {children}
      <Tasks artifact={artifact} />
    </section>
  );
}
