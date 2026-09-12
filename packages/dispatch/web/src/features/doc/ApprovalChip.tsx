import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useId, useRef, useState } from "react";

import { api, type CreateArtifactReviewInput } from "../../api/client";
import type { Artifact, ArtifactApproval } from "../../api/types";
import { useSubmitGuard } from "../../hooks/useSubmitGuard";
import {
  backdrop40,
  badgeBlocking,
  badgeHigh,
  badgeLow,
  badgeMed,
  card,
  dangerText,
  inputClasses,
  primaryButtonBg,
  primaryButtonDisabled,
  primaryButtonEnabledHoverBg,
  secondaryButtonBorder,
  secondaryButtonHoverBorder,
  secondaryButtonText,
  textMutedOnSurface,
  textPrimaryOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { actorLabel } from "../refs/actor";
import { MarkdownBody } from "../refs/MarkdownBody";
import { Timestamp } from "../refs/Timestamp";
import { useDialog } from "../shell/useDialog";

const STATE_BADGE: Record<ArtifactApproval["state"], { bg: string; text: string }> = {
  approved: badgeLow,
  awaiting: badgeMed,
  changes_requested: badgeBlocking,
  draft: badgeLow,
  stale: badgeHigh,
};

function approvalLabel(approval: ArtifactApproval): string {
  switch (approval.state) {
    case "draft":
      return "Draft";
    case "awaiting":
      return "Awaiting approval";
    case "approved":
      return `Approved v${approval.version ?? approval.latest_version}`;
    case "stale":
      return `Approved v${approval.version ?? approval.latest_version} · changed since`;
    case "changes_requested":
      return "Changes requested";
  }
}

function ReviewsDialog({
  artifactId,
  onClose,
}: {
  artifactId: string;
  onClose: () => void;
}): ReactNode {
  const dialog = useDialog<HTMLDivElement>({ onClose, open: true });
  const reviews = useQuery({
    queryFn: () => api.listArtifactReviews(artifactId),
    queryKey: ["artifact-reviews", artifactId],
  });

  return (
    <>
      <div aria-hidden="true" className={`fixed inset-0 z-40 ${backdrop40}`} onClick={onClose} />
      <div className="pointer-events-none fixed inset-0 z-40 flex items-start justify-center p-4 pt-20">
        <div
          aria-label="Reviews"
          aria-modal="true"
          className={`pointer-events-auto w-full max-w-md space-y-3 rounded-lg border p-4 shadow-xl ${card}`}
          ref={dialog.containerRef}
          role="dialog"
        >
          <div className="flex items-center justify-between gap-3">
            <h2 className={`text-sm font-semibold ${textSecondaryOnSurface}`}>Reviews</h2>
            <button
              className={`text-sm font-medium ${textMutedOnSurface}`}
              onClick={onClose}
              type="button"
            >
              Close
            </button>
          </div>
          {reviews.isPending ? (
            <p className={`text-sm ${textMutedOnSurface}`}>Loading reviews…</p>
          ) : reviews.isError ? (
            <p className={`text-sm ${dangerText}`}>Could not load reviews.</p>
          ) : reviews.data.length === 0 ? (
            <p className={`text-sm ${textMutedOnSurface}`}>No reviews yet.</p>
          ) : (
            <ul className="space-y-3">
              {reviews.data.map((review) => (
                <li key={review.id}>
                  <p className={`text-sm font-medium ${textPrimaryOnSurface}`}>
                    {actorLabel(review.actor)}{" "}
                    {review.state === "approved" ? "approved" : "requested changes on"} v
                    {review.version} · <Timestamp at={review.created_at} />
                  </p>
                  {review.reason === null ? null : (
                    <div className={`mt-1 text-sm ${textSecondaryOnSurface}`}>
                      <MarkdownBody markdown={review.reason} />
                    </div>
                  )}
                </li>
              ))}
            </ul>
          )}
        </div>
      </div>
    </>
  );
}

function RequestChangesDialog({
  error,
  onClose,
  onSubmit,
  saving,
}: {
  error: boolean;
  onClose: () => void;
  onSubmit: (reason: string) => void;
  saving: boolean;
}): ReactNode {
  const [reason, setReason] = useState("");
  const reasonId = useId();
  const textareaRef = useRef<HTMLTextAreaElement>(null);
  const dialog = useDialog<HTMLDivElement>({ initialFocusRef: textareaRef, onClose, open: true });
  const trimmed = reason.trim();

  return (
    <>
      <div aria-hidden="true" className={`fixed inset-0 z-40 ${backdrop40}`} onClick={onClose} />
      <div className="pointer-events-none fixed inset-0 z-40 flex items-start justify-center p-4 pt-20">
        <div
          aria-label="Request changes"
          aria-modal="true"
          className={`pointer-events-auto w-full max-w-sm space-y-3 rounded-lg border p-4 shadow-xl ${card}`}
          ref={dialog.containerRef}
          role="dialog"
        >
          <h2 className={`text-sm font-semibold ${textSecondaryOnSurface}`}>Request changes</h2>
          <label
            className={`block text-sm font-medium ${textSecondaryOnSurface}`}
            htmlFor={reasonId}
          >
            Reason
            <textarea
              className={`mt-1 block w-full rounded-lg border px-2 py-1.5 text-sm font-normal outline-none ${inputClasses(false)}`}
              disabled={saving}
              id={reasonId}
              onChange={(event) => setReason(event.target.value)}
              ref={textareaRef}
              value={reason}
            />
          </label>
          {error ? (
            <p className={`text-sm ${dangerText}`} role="alert">
              Could not request changes. Try again.
            </p>
          ) : null}
          <div className="flex items-center justify-end gap-2">
            <button
              className={`rounded-lg border px-3 py-1.5 text-sm font-medium ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder}`}
              disabled={saving}
              onClick={onClose}
              type="button"
            >
              Cancel
            </button>
            <button
              className={`rounded-lg px-3 py-1.5 text-sm font-medium ${primaryButtonBg} ${primaryButtonEnabledHoverBg} ${primaryButtonDisabled}`}
              disabled={trimmed === "" || saving}
              onClick={() => onSubmit(trimmed)}
              type="button"
            >
              {saving ? "Submitting…" : "Request changes"}
            </button>
          </div>
        </div>
      </div>
    </>
  );
}

export interface ApprovalChipProps {
  artifact: Artifact;
  /** `header`: shown on the document header, with Approve/Request changes controls.
   *  `list` (default): shown in a compact list row (Artifacts tab, project Documents tab) —
   *  a draft document renders nothing, staying quiet until it has something to report. */
  variant?: "header" | "list";
}

/**
 * The document approval indicator: a small badge summarizing `artifact.approval`, clickable to
 * open a popover of every human review (`ArtifactReview`). On the document header only, it also
 * carries the Approve / Request changes actions that write a new review.
 */
export function ApprovalChip({ artifact, variant = "list" }: ApprovalChipProps): ReactNode {
  const { approval } = artifact;
  const [reviewsOpen, setReviewsOpen] = useState(false);
  const [requestChangesOpen, setRequestChangesOpen] = useState(false);
  const queryClient = useQueryClient();
  const submitGuard = useSubmitGuard();

  const review = useMutation({
    mutationFn: (input: CreateArtifactReviewInput) => api.createArtifactReview(artifact.id, input),
    onSettled: () => submitGuard.release(),
    onSuccess: () => {
      setRequestChangesOpen(false);
      void queryClient.invalidateQueries({ queryKey: ["artifact-reviews", artifact.id] });
      void queryClient.invalidateQueries({ queryKey: ["artifact", artifact.id] });
      void queryClient.invalidateQueries({ queryKey: ["inbox"] });
      if (artifact.issue_key === null) {
        void queryClient.invalidateQueries({
          queryKey: ["project", artifact.project, "artifacts"],
        });
        void queryClient.invalidateQueries({
          queryKey: ["artifact-ref", `${artifact.project}/${artifact.slug}`],
        });
        return;
      }
      void queryClient.invalidateQueries({ queryKey: ["issue", artifact.issue_key] });
      void queryClient.invalidateQueries({ queryKey: ["artifacts", artifact.issue_key] });
      void queryClient.invalidateQueries({ queryKey: ["asks", artifact.issue_key] });
    },
  });

  if (approval === undefined) {
    return null;
  }
  if (approval.state === "draft" && variant === "list") {
    return null;
  }

  const badge = STATE_BADGE[approval.state];
  const approveLabel =
    approval.state === "stale" ? `Approve v${approval.latest_version}` : "Approve";

  return (
    <div className="inline-flex flex-wrap items-center gap-2">
      <button
        aria-haspopup="dialog"
        className={`rounded-full px-2.5 py-1 text-xs font-medium ${badge.bg} ${badge.text}`}
        onClick={() => setReviewsOpen(true)}
        title={
          approval.state === "stale"
            ? `Approved at version ${approval.version}; the latest version is ${approval.latest_version}`
            : undefined
        }
        type="button"
      >
        {approvalLabel(approval)}
      </button>
      {variant === "header" ? (
        <>
          {approval.state === "approved" ? null : (
            <button
              className={`rounded-lg border px-3 py-2 text-sm font-medium ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder}`}
              disabled={review.isPending}
              onClick={() => submitGuard.guard(() => review.mutate({ state: "approved" }))}
              type="button"
            >
              {approveLabel}
            </button>
          )}
          <button
            className={`rounded-lg border px-3 py-2 text-sm font-medium ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder}`}
            disabled={review.isPending}
            onClick={() => setRequestChangesOpen(true)}
            type="button"
          >
            Request changes
          </button>
        </>
      ) : null}
      {reviewsOpen ? (
        <ReviewsDialog artifactId={artifact.id} onClose={() => setReviewsOpen(false)} />
      ) : null}
      {requestChangesOpen ? (
        <RequestChangesDialog
          error={review.isError}
          onClose={() => {
            review.reset();
            setRequestChangesOpen(false);
          }}
          onSubmit={(reason) =>
            submitGuard.guard(() => review.mutate({ reason, state: "changes_requested" }))
          }
          saving={review.isPending}
        />
      ) : null}
    </div>
  );
}
