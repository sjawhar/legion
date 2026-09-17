import { useMutation, useQueryClient } from "@tanstack/react-query";

import { api } from "../../api/client";
import type {
  ArchitectureTree,
  ArchitectureTreeIssueRef,
  IssueComponents,
  IssueComponentsInput,
  IssueDetails,
} from "../../api/types";
import { predictTree } from "../architecture/architecture-model";

export interface IssueComponentsWrite {
  /** The failed save's message, until the next attempt starts. */
  error: string | null;
  pending: boolean;
  /** Saves the attachment; resolves `true` once the server accepted it. */
  submit: (input: IssueComponentsInput) => Promise<boolean>;
}

interface Snapshot {
  details: IssueDetails | undefined;
  tree: ArchitectureTree | undefined;
}

/** The attachment `input` resolves to when the server accepts it, for the issue detail cache;
 *  `inherit` depends on the ancestors, so the detail keeps its current value until the refetch. */
function predictComponents(input: IssueComponentsInput): IssueComponents | undefined {
  if (input.mode === "explicit") {
    return {
      ids: input.ids ?? [],
      inherited_from: null,
      mode: "explicit",
      reason: null,
      unknown: [],
    };
  }
  if (input.mode === "none") {
    return { ids: [], inherited_from: null, mode: "none", reason: input.reason ?? "", unknown: [] };
  }
  return undefined;
}

/**
 * The one way the SPA writes an issue's component attachment (`PATCH /issues/{key}`
 * `components`), from the Architecture pane's Unassigned and Not architectural lists and from
 * the issue header's editor: the new attachment lands optimistically in the issue detail (when
 * that cache exists) and in the project's architecture tree (the issue moves between lists and
 * rows, the counts with it — `predictTree`), rolls back on failure, and both refetch once the
 * server has answered so descendants that inherit the change appear too.
 */
export function useIssueComponents(
  issue: ArchitectureTreeIssueRef & { project: string }
): IssueComponentsWrite {
  const queryClient = useQueryClient();
  const detailKey = ["issue", issue.key] as const;
  const treeKey = ["architecture", issue.project] as const;
  const mutation = useMutation({
    mutationFn: (input: IssueComponentsInput) => api.patchIssue(issue.key, { components: input }),
    onMutate: async (input): Promise<Snapshot> => {
      await Promise.all([
        queryClient.cancelQueries({ queryKey: detailKey }),
        queryClient.cancelQueries({ queryKey: treeKey }),
      ]);
      const snapshot: Snapshot = {
        details: queryClient.getQueryData<IssueDetails>(detailKey),
        tree: queryClient.getQueryData<ArchitectureTree>(treeKey),
      };
      const components = predictComponents(input);
      if (components !== undefined) {
        queryClient.setQueryData<IssueDetails>(detailKey, (current) =>
          current === undefined ? undefined : { ...current, components }
        );
      }
      queryClient.setQueryData<ArchitectureTree>(treeKey, (current) =>
        current === undefined
          ? undefined
          : predictTree(current, issue, input, new Date().toISOString())
      );
      return snapshot;
    },
    onError: (_error, _input, snapshot) => {
      if (snapshot?.details !== undefined) {
        queryClient.setQueryData<IssueDetails>(detailKey, snapshot.details);
      }
      if (snapshot?.tree !== undefined) {
        queryClient.setQueryData<ArchitectureTree>(treeKey, snapshot.tree);
      }
    },
    onSettled: () => {
      void queryClient.invalidateQueries({ queryKey: treeKey });
      // Every open issue, this one included: descendants inherit the change with no event of
      // their own.
      void queryClient.invalidateQueries({ queryKey: ["issue"] });
    },
  });
  return {
    error: mutation.error === null ? null : mutation.error.message,
    pending: mutation.isPending,
    submit: async (input) => {
      try {
        await mutation.mutateAsync(input);
        return true;
      } catch {
        return false;
      }
    },
  };
}
