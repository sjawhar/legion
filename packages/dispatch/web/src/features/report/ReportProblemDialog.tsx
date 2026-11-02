import { isPictureType } from "@legion/contracts";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import {
  type ClipboardEvent,
  type DragEvent,
  type FormEvent,
  type ReactNode,
  useEffect,
  useRef,
  useState,
} from "react";
import { Link } from "react-router-dom";

import { ApiError, api, type WriteBounds } from "../../api/client";
import { issueQuery, reportSettingsQuery } from "../../api/queries";
import type { ReportInput, ReportResponse } from "../../api/types";
import { QueryError } from "../../components/QueryError";
import { submitOnModifiedEnter } from "../../hooks/submitOnModifiedEnter";
import { useSubmitGuard } from "../../hooks/useSubmitGuard";
import {
  backdrop50,
  borderDefault,
  borderStrong,
  card,
  checkboxAccent,
  dangerText,
  focusRing,
  inputClasses,
  linkHoverText,
  linkText,
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
import { buildIssuePath } from "../refs/routes";
import { useCloseOnNavigation, useDialog } from "../shell/useDialog";
import { LandedWithoutAnswer } from "../writes/LandedWithoutAnswer";
import { useKeyedWrite } from "../writes/useKeyedWrite";
import { type ReportPageContext, reportContext } from "./report-context";

const createReport = (
  input: ReportInput,
  pictures: readonly File[],
  bounds: WriteBounds
): Promise<ReportResponse> => api.createReport(input, pictures, bounds);

interface ReportPress {
  readonly input: ReportInput;
  readonly pictures: readonly File[];
}

/** What the reporter is told about the agent once the report is filed with one asked: whether a
 *  session held its role, as the new issue's read says (`route_status`), once it has. */
function agentNote(route: string | null, status: string | null | undefined): string | null {
  if (route === null || status === undefined) {
    return null;
  }
  switch (status) {
    case "live":
      return `The agent holding ${route} has it.`;
    case "no_holder":
      return `No agent holds ${route} right now, so none was told. The issue keeps the request: what anyone adds to it reaches the agent once one holds the role.`;
    default:
      return `Dispatch could not tell whether an agent holds ${route}; the issue keeps the request.`;
  }
}

/**
 * **Report a problem**: the `?` panel's button and the palette's action open it on any page. The
 * person says what went wrong and may paste or drop pictures into the box, as into a comment;
 * Send files an issue in the project Settings names for reports, carrying the words, the pictures
 * and where it happened (`reportContext`), and the dialog then names the new issue. When Settings
 * names an agent, a box asks it to fix the problem. Mounted only while open, so each opening
 * starts a fresh draft.
 *
 * A comment's paste uploads each picture to the issue it is on the moment it is pasted
 * (`useDraftUpload`); a report's issue does not exist until Send, so its pictures stay in the
 * form and go with the report, which writes them as files of the new issue.
 */
export function ReportProblemDialog({
  onClose,
  context = reportContext,
  createReport: create = createReport,
}: {
  onClose: () => void;
  /** The page's details, read at Send; a seam for tests. */
  context?: () => ReportPageContext;
  /** Write seam for tests; defaults to the real API. */
  createReport?: (
    input: ReportInput,
    pictures: readonly File[],
    bounds: WriteBounds
  ) => Promise<ReportResponse>;
}): ReactNode {
  const queryClient = useQueryClient();
  const fieldRef = useRef<HTMLTextAreaElement>(null);
  const closeRef = useRef<HTMLButtonElement>(null);
  const settings = useQuery(reportSettingsQuery());
  const configured = settings.data?.project != null;
  const dialog = useDialog<HTMLFormElement>({
    initialFocusRef: configured ? fieldRef : closeRef,
    onClose,
    open: true,
  });
  useCloseOnNavigation(true, onClose);
  // The dialog opens before it knows reports are set up, on Close; the box takes focus once it
  // shows, unless the reader has already moved focus elsewhere in the dialog.
  useEffect(() => {
    if (
      configured &&
      (document.activeElement === closeRef.current || document.activeElement === document.body)
    ) {
      fieldRef.current?.focus();
    }
  }, [configured]);
  const [body, setBody] = useState("");
  const [pictures, setPictures] = useState<readonly File[]>([]);
  const [refused, setRefused] = useState<readonly string[]>([]);
  const [askAgent, setAskAgent] = useState(false);
  const [filed, setFiled] = useState<ReportResponse | null>(null);
  const filedIssue = useQuery({
    ...issueQuery(filed?.key),
    enabled: filed !== null && filed.route !== null,
  });
  const submitGuard = useSubmitGuard();
  const mutation = useKeyedWrite({
    // The write store keeps one file with a write across a reload, so a report of one picture is
    // sent again whole after one. One of several pictures is not kept: such a report left out
    // when the page reloads is let go, after the page's own prompt before leaving.
    file: ({ pictures: sent }: ReportPress) => (sent.length === 1 ? sent[0] : undefined),
    inline: true,
    label: () => "Problem report",
    onSettled: () => submitGuard.release(),
    write: ({ input, pictures: sent }: ReportPress, bounds: WriteBounds) =>
      create(input, sent, bounds),
    onSuccess: (report) => {
      void queryClient.invalidateQueries({ queryKey: ["issues"] });
      // Made either way; with no answer there is no key to show, and the dialog says what was
      // made (`mutation.unanswered`). The draft goes, so a second press makes nothing.
      setBody("");
      setPictures([]);
      setRefused([]);
      if (report !== null) {
        setFiled(report);
      }
    },
  });

  const agentRoute = settings.data?.agent_route ?? null;
  const canSubmit = configured && body.trim() !== "" && !mutation.isPending;
  const receive = (files: FileList, event: { preventDefault(): void }) => {
    if (files.length === 0) {
      return;
    }
    event.preventDefault();
    mutation.forgetUnanswered();
    const all = [...files];
    setPictures((held) => [...held, ...all.filter((file) => isPictureType(file.type))]);
    setRefused(
      all.filter((file) => !isPictureType(file.type)).map((file) => file.name || "a file")
    );
  };
  const handleSubmit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (!canSubmit) {
      return;
    }
    submitGuard.guard(() =>
      mutation.mutate({
        input: { ...context(), ask_agent: agentRoute !== null && askAgent, body },
        pictures,
      })
    );
  };

  const buttonSecondary = `min-h-11 rounded-lg border px-3 py-2 text-sm font-medium ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder}`;
  const link = `font-medium underline ${linkText} ${linkHoverText}`;
  let content: ReactNode;
  if (settings.isPending) {
    content = <p className={`text-sm ${textMutedOnSurface}`}>Loading…</p>;
  } else if (settings.isError) {
    content = (
      <QueryError
        message="Could not read where reports go."
        onRetry={() => void settings.refetch()}
        retrying={settings.isFetching}
      />
    );
  } else if (!configured) {
    content = (
      <p className={`text-sm ${textSecondaryOnSurface}`}>
        Reports are not set up yet. Someone who looks after this Dispatch can choose the project
        reports go to in{" "}
        <Link className={link} to="/settings#problem-reports">
          Settings → Problem reports
        </Link>
        .
      </p>
    );
  } else if (filed !== null) {
    const note = agentNote(filed.route, filedIssue.data?.route_status);
    content = (
      <div className="space-y-2" role="status">
        <p className={`text-sm ${textSecondaryOnSurface}`}>
          Reported as{" "}
          <Link className={link} to={buildIssuePath({ key: filed.key, kind: "issue" })}>
            {filed.key}
          </Link>
          . Thank you.
        </p>
        {note === null ? null : <p className={`text-sm ${textSecondaryOnSurface}`}>{note}</p>}
      </div>
    );
  } else {
    content = (
      <>
        <label className={`block text-sm font-medium ${textSecondaryOnSurface}`}>
          What went wrong
          <textarea
            className={`mt-1 block w-full rounded-lg border px-3 py-2 text-sm font-normal outline-none ${inputClasses(true)}`}
            disabled={mutation.isPending}
            onChange={(event) => {
              mutation.forgetUnanswered();
              setBody(event.target.value);
            }}
            onDragOver={(event: DragEvent<HTMLTextAreaElement>) => {
              if (event.dataTransfer.types.includes("Files")) event.preventDefault();
            }}
            onDrop={(event: DragEvent<HTMLTextAreaElement>) =>
              receive(event.dataTransfer.files, event)
            }
            onKeyDown={(event) => submitOnModifiedEnter(event, { disabled: !canSubmit })}
            onPaste={(event: ClipboardEvent<HTMLTextAreaElement>) =>
              receive(event.clipboardData.files, event)
            }
            ref={fieldRef}
            rows={5}
            value={body}
          />
        </label>
        <p className={`text-xs ${textMutedOnSurface}`}>
          Paste or drop a picture to show what you saw. The report also says which page this is, the
          version of Dispatch, your browser and your window's size.
        </p>
        {pictures.length === 0 ? null : (
          <ul aria-label="Pictures" className="space-y-1">
            {pictures.map((picture, index) => (
              <li
                className={`flex items-center gap-2 text-sm ${textSecondaryOnSurface}`}
                // A list of one person's pastes, never reordered: its place names it.
                // biome-ignore lint/suspicious/noArrayIndexKey: two pastes may share a name
                key={index}
              >
                <span className="min-w-0 flex-1 truncate">{picture.name}</span>
                <button
                  aria-label={`Remove ${picture.name}`}
                  className={`min-h-11 rounded-lg px-2 text-sm ${textMutedOnSurface}`}
                  disabled={mutation.isPending}
                  onClick={() => setPictures((held) => held.filter((_, at) => at !== index))}
                  type="button"
                >
                  Remove
                </button>
              </li>
            ))}
          </ul>
        )}
        {refused.length === 0 ? null : (
          <p className={`text-sm ${dangerText}`} role="alert">
            {refused.join(", ")} {refused.length === 1 ? "is" : "are"} not a picture. A report takes
            PNG, JPEG, GIF or WebP pictures.
          </p>
        )}
        {agentRoute === null ? null : (
          <label
            className={`flex items-center gap-2 text-sm font-medium ${textSecondaryOnSurface}`}
          >
            <input
              checked={askAgent}
              className={`size-4 rounded ${borderStrong} ${checkboxAccent} ${focusRing}`}
              disabled={mutation.isPending}
              onChange={(event) => setAskAgent(event.target.checked)}
              type="checkbox"
            />
            Ask an agent to fix this
          </label>
        )}
        {mutation.unanswered === undefined ? null : <LandedWithoutAnswer what="Your report" />}
        {mutation.isError ? (
          <QueryError
            message={
              mutation.error instanceof ApiError
                ? mutation.error.message
                : "Could not send the report."
            }
            onRetry={() => submitGuard.retryLast(mutation)}
            retrying={mutation.isPending}
          />
        ) : null}
      </>
    );
  }
  const composing = configured && filed === null && !settings.isPending && !settings.isError;

  return (
    <>
      <div aria-hidden="true" className={`fixed inset-0 z-40 ${backdrop50}`} onClick={onClose} />
      <div className="pointer-events-none fixed inset-0 z-50 flex items-start justify-center xl:p-4 xl:pt-20">
        <form
          aria-label="Report a problem"
          aria-modal="true"
          className={`pointer-events-auto max-h-[100dvh] w-full space-y-3 overflow-y-auto border p-4 shadow-2xl xl:max-h-[calc(100dvh-6rem)] xl:max-w-lg xl:rounded-lg ${card} ${borderDefault}`}
          onSubmit={handleSubmit}
          ref={dialog.containerRef}
          role="dialog"
        >
          <h2 className={`text-base font-semibold ${textPrimaryOnSurface}`}>Report a problem</h2>
          {content}
          <div className="flex items-center justify-end gap-2">
            <button className={buttonSecondary} onClick={onClose} ref={closeRef} type="button">
              {composing ? "Cancel" : "Close"}
            </button>
            {composing ? (
              <button
                className={`min-h-11 rounded-lg px-3 py-2 text-sm font-semibold ${primaryButtonBg} ${primaryButtonEnabledHoverBg} ${primaryButtonDisabled}`}
                disabled={!canSubmit}
                type="submit"
              >
                {mutation.isPending ? "Sending…" : "Send report"}
              </button>
            ) : null}
          </div>
        </form>
      </div>
    </>
  );
}
