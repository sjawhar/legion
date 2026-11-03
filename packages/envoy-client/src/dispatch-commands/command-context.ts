import type { Actor, ArtifactUploadResponse, CreateArtifactInput } from "@legion/contracts";
import { type DispatchOrigin, defaultExec, resolveOrigin } from "../dispatch-cwd";
import { DispatchWriteLandedError } from "../dispatch-http";
import { type PictureTextLimit, shownPictures, textWithPictures } from "../dispatch-picture-tools";
import { ToolInputError } from "../tool-input-errors";
import {
  type CommandContext,
  type ExecuteDispatchToolInput,
  optionalString,
  type ResolvedArtifact,
  type SessionContext,
} from "./context";
import { documentTargets } from "./document-targets";

function toolActor(origin: DispatchOrigin, input: ExecuteDispatchToolInput): Actor {
  return {
    kind: "session",
    id: input.sessionId ?? "unknown",
    origin: {
      ...origin,
      host: input.host,
      ...(input.sessionTitle === undefined ? {} : { session_title: input.sessionTitle }),
    },
  };
}

/**
 * The context a command that acts for this session runs on, built once per call: the origin a
 * write is recorded against (a subprocess, which is why no command that answers without one
 * reaches this), the client every request goes through, the owner the call names with Dispatch's
 * own key for it, and the pictures helpers a write posts with.
 */
export async function commandContext(session: SessionContext): Promise<CommandContext> {
  const { input, ownerArguments, pictures } = session;
  const actor = toolActor(
    await resolveOrigin(input.env ?? process.env, input.exec ?? defaultExec, input.cwd),
    input
  );
  const client = session.newClient();
  const { owner, documentOwner, resolveDocument } = await documentTargets(client, ownerArguments);
  // An external issue reference beside an issue ref is compared here, once Dispatch has resolved
  // it to the key the ref spells (`ownerConflictProblems` compares native keys before any request).
  const issueArgument = optionalString(input.args, "issue");
  const refText = optionalString(ownerArguments.args, "ref");
  if (
    input.tool !== "dispatch_ask" &&
    owner?.kind === "issue" &&
    issueArgument !== undefined &&
    refText !== undefined &&
    ownerArguments.ref?.owner.kind === "issue" &&
    owner.issue !== ownerArguments.ref.owner.issue
  ) {
    throw new ToolInputError(input.tool, [
      `issue ${issueArgument} and ref ${refText} name two owners; --ref ${refText} alone acts on ${ownerArguments.ref.owner.issue}, and --issue ${issueArgument} alone acts on ${owner.issue}`,
    ]);
  }
  const issue = () => {
    if (owner?.kind !== "issue") throw new Error("issue is required");
    return owner.issue;
  };
  const postWithPictures = (
    text: string,
    limit: PictureTextLimit,
    owner: string,
    upload: (upload: CreateArtifactInput) => Promise<ArtifactUploadResponse>
  ): Promise<string> =>
    textWithPictures(
      input.tool,
      text,
      pictures,
      limit,
      owner,
      (picture) =>
        upload(picture).catch((error: unknown) => {
          if (!(error instanceof DispatchWriteLandedError)) throw error;
          throw new Error(
            `${error.message} That was a picture uploaded before this call's own write, so nothing else was written: run the call again, which uploads it as its next version.`,
            { cause: error }
          );
        }),
      actor
    );
  const writePictures = (
    resolved: ResolvedArtifact | undefined,
    text: string,
    limit: PictureTextLimit
  ): Promise<string> => {
    if (resolved?.owner.kind === "project") {
      const { project } = resolved.owner;
      return postWithPictures(text, limit, project, (upload) =>
        client.projectArtifact(project, upload)
      );
    }
    const issueKey = issue();
    return postWithPictures(text, limit, issueKey, (upload) => client.artifact(issueKey, upload));
  };
  const { hostSession } = session;
  return {
    ...session,
    client,
    actor,
    owner,
    documentOwner,
    resolveDocument,
    issue,
    postWithPictures,
    writePictures,
    shown: hostSession ? shownPictures(hostSession) : undefined,
  };
}
