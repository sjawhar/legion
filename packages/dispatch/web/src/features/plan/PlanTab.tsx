import { parseBlockPointerValue } from "@legion/contracts";
import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useMemo } from "react";

import { artifactBlocksQuery } from "../../api/queries";
import type { Artifact, ArtifactBlock } from "../../api/types";
import { EmptyState } from "../../components/EmptyState";
import { card, dangerText, textMutedOnCanvas, textMutedOnSurface } from "../../theme/classes";
import { ApprovalChip } from "../doc/ApprovalChip";
import { withBlock } from "../refs/block-reference";
import { BlockReferences } from "../refs/ReferencedBy";
import { ReferenceLink } from "../refs/ReferenceLink";
import { useReferenceTarget } from "../refs/reference-target";
import { buildDispatchReference, documentRoute, parseNodeReference } from "../refs/routes";

/** The `block[]` attribute `name` of `block` as the pointers it holds; empty for a block that
 *  carries none and for a value the server would refuse, which no stored block holds. */
function pointers(block: ArtifactBlock, name: string): readonly string[] {
  const value = block.attributes?.find((attribute) => attribute.name === name)?.value;
  return value === undefined ? [] : (parseBlockPointerValue(value) ?? []);
}

/** A block a pointer names, drawn as its own words once the graph answers them and as the
 *  reference's short form until then; a pointer that names nothing shows as the text it is. */
function BlockLink({ reference }: { reference: string }): ReactNode {
  const { title } = useReferenceTarget(parseNodeReference(reference), { text: false });
  return <ReferenceLink label={title} reference={reference} />;
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

/** One task of the plan: its own link, what it serves, what it needs, and what points at it. */
function Task({ task }: { task: PlanTask }): ReactNode {
  return (
    <li className={`space-y-2 rounded-lg p-3 ${card}`} data-testid="plan-task">
      <BlockLink reference={task.reference} />
      <Pointers label="Serves" references={task.serves} testId="plan-task-serves" />
      <Pointers label="Needs" references={task.needs} testId="plan-task-needs" />
      <BlockReferences reference={task.reference} />
    </li>
  );
}

/** One `plan_task` of the plan as the list draws it: its reference and its two pointer sets,
 *  each parsed once per read of the blocks rather than on every render of the tab, which stays
 *  mounted and re-renders on every live event of the issue. */
interface PlanTask {
  readonly id: string;
  readonly reference: string;
  readonly serves: readonly string[];
  readonly needs: readonly string[];
}

function Tasks({ artifact }: { artifact: Artifact }): ReactNode {
  // The same key the document's own block read uses, so a tab opened after the document is
  // already answered.
  const blocks = useQuery(artifactBlocksQuery(artifact.id));
  const data = blocks.data;
  const tasks = useMemo<readonly PlanTask[]>(() => {
    if (data === undefined) {
      return [];
    }
    const document = documentRoute(artifact);
    return data
      .filter((block) => block.type === "plan_task")
      .flatMap((block) => {
        const route = withBlock(document, block.id);
        return route === undefined
          ? []
          : [
              {
                id: block.id,
                reference: buildDispatchReference(route),
                serves: pointers(block, "serves"),
                needs: pointers(block, "needs"),
              },
            ];
      });
  }, [artifact, data]);

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
  if (tasks.length === 0) {
    return (
      <div data-testid="plan-no-tasks">
        <EmptyState label="Plan tasks empty state" message="This plan marks no tasks." />
      </div>
    );
  }

  return (
    <ul aria-label="Plan tasks" className="space-y-3" data-testid="plan-tasks">
      {tasks.map((task) => (
        <Task key={task.id} task={task} />
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
