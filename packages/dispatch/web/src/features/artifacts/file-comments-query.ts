import { useQuery } from "@tanstack/react-query";

import { api } from "../../api/client";
import type { Comment } from "../../api/types";
import type { ComposerOwner } from "../conversation/composer-model";
import type { MarginOwner } from "../margin/useMarginItems";
import { fileBoxNumbers, isFileAnchor } from "./picture-box";

/** Where a file's comments are kept: its issue, or the project file itself. */
export type FileCommentOwner = Exclude<ComposerOwner, { kind: "session" }>;

/** The comments of the file's owner, under the very key the margin and the Conversation read them
 *  by, so the file page, the margin's cards and the Conversation's share one read and one refresh
 *  (`held-sends.ts`'s `refreshAfter` invalidates it when a send lands). */
export function fileCommentsQuery(owner: FileCommentOwner): {
  queryFn: () => Promise<Comment[]>;
  queryKey: readonly string[];
} {
  return owner.kind === "issue"
    ? {
        queryFn: () => api.listComments(owner.issueKey),
        queryKey: ["comments", owner.issueKey],
      }
    : {
        queryFn: () => api.listArtifactComments(owner.artifactId),
        queryKey: ["artifact", owner.artifactId, "comments"],
      };
}

/** The composer owner of a margin's owner. */
function fileCommentOwner(owner: MarginOwner): FileCommentOwner {
  return owner.kind === "issue"
    ? { issueKey: owner.key, kind: "issue" }
    : { artifactId: owner.artifactId, kind: "artifact", project: owner.project };
}

/**
 * The number `comment`'s box shows over its picture, for a card that renders the thread: the
 * margin's, the phone thread's and the Conversation's each read it here, from the owner's comments
 * every one of them already holds. Undefined for any comment that is not a box on a file.
 */
export function useFileBoxNumber(
  owner: MarginOwner,
  comment: Pick<Comment, "anchor" | "id">
): number | undefined {
  const anchor = comment.anchor;
  const onBox = isFileAnchor(anchor) && anchor.box !== undefined;
  const comments = useQuery({ ...fileCommentsQuery(fileCommentOwner(owner)), enabled: onBox });
  if (!onBox) return undefined;
  return fileBoxNumbers(comments.data ?? [], anchor.artifact_id).get(comment.id);
}
