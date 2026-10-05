import type { CredentialSession } from "../../api/types";
import { useAgents } from "../conversation/useAgents";
import {
  type CredentialSessionLine,
  credentialSessionLines,
  credentialSessionNamesAnyone,
} from "./session";

/** Resolves a credential request's `session` field against the live agents list, for the Inbox
 *  row and the record page to render identically. Enables `useAgents` only when there is an id to
 *  resolve, so a request that names no session never polls the agents list for it. */
export function useCredentialSessionLines(
  session: CredentialSession | undefined
): readonly CredentialSessionLine[] {
  const { agents, isError, isPending } = useAgents(credentialSessionNamesAnyone(session));
  return credentialSessionLines(session, agents, isError || isPending);
}
