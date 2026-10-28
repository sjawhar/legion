import type { Comment, CommentDraft, Pair, Receipt } from "./inputs.ts";

/** Pairs each comments.csv pair with its one listed receipts.tsv row; refuses a pair with none,
 *  a listed row with no pair, a row whose comment, listed and target repeat, and a row whose event
 *  is not its comment's. */
export function join(
  drafts: readonly CommentDraft[],
  receipts: readonly Receipt[],
  problems: string[]
): Comment[] {
  const byComment = new Map(drafts.map((comment) => [comment.id, comment]));
  const listed = new Map<string, Receipt>();
  const unlisted = new Map<string, Receipt>();
  for (const receipt of receipts) {
    const comment = byComment.get(receipt.commentId);
    if (comment === undefined) {
      problems.push(
        `receipts.tsv line ${receipt.line}: comment ${receipt.commentId} is not in comments.csv`
      );
      continue;
    }
    if (receipt.eventId !== comment.eventId) {
      problems.push(
        `receipts.tsv line ${receipt.line}: comment ${receipt.commentId} with event ` +
          `"${receipt.eventId}", but comments.csv gives event "${comment.eventId}"`
      );
    }
    const [seen, what] = receipt.listed ? [listed, "target"] : [unlisted, "unlisted target"];
    const key = `${receipt.commentId}\t${receipt.target}`;
    const earlier = seen.get(key);
    if (earlier !== undefined) {
      problems.push(
        `receipts.tsv lines ${earlier.line} and ${receipt.line}: comment ${receipt.commentId} and ${what} "${receipt.target}" twice`
      );
    }
    seen.set(key, receipt);
  }
  const paired = new Set<string>();
  const comments: Comment[] = [];
  for (const draft of drafts) {
    const pairs: Pair[] = [];
    for (const { line, ...pair } of draft.pairs) {
      const key = `${draft.id}\t${pair.target}`;
      const receipt = listed.get(key);
      if (receipt === undefined) {
        problems.push(
          `comments.csv line ${line}: comment ${draft.id} and target ${pair.target} have no listed row in receipts.tsv`
        );
        continue;
      }
      paired.add(key);
      pairs.push({ ...pair, receipt });
    }
    comments.push({ ...draft, pairs });
  }
  for (const [key, receipt] of listed) {
    if (!paired.has(key)) {
      problems.push(
        `receipts.tsv line ${receipt.line}: comment ${receipt.commentId} and target ` +
          `"${receipt.target}" is a listed row that comments.csv does not hold`
      );
    }
  }
  return comments;
}
