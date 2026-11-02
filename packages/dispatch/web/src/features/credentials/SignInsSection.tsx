import { useQuery, useQueryClient } from "@tanstack/react-query";
import type { ReactNode } from "react";

import { api } from "../../api/client";
import type { SignIn } from "../../api/types";
import { textSecondaryOnSurface } from "../../theme/classes";
import { Timestamp } from "../refs/Timestamp";
import { type RevocableColumn, RevocableList } from "./RevocableList";
import { shortThumbprint, signInsQuery } from "./signins";

const columns: readonly RevocableColumn<SignIn>[] = [
  {
    cell: (signIn) => (
      <>
        {signIn.machine}
        <span
          className={`block font-mono text-xs font-normal ${textSecondaryOnSurface}`}
          title={signIn.thumbprint}
        >
          key {shortThumbprint(signIn.thumbprint)}
        </span>
      </>
    ),
    header: "Machine",
  },
  { cell: (signIn) => <Timestamp at={signIn.created_at} />, header: "Signed in" },
  { cell: (signIn) => <Timestamp at={signIn.last_used_at} />, header: "Last used" },
];

/**
 * The viewer's own sign-ins: one row per machine they ran `agent-secrets login` on and have not
 * revoked. Nobody sees anyone else's — Dispatch names the viewer from its own session, and the
 * broker lists that person's alone. Each row names the machine by the account its fleet signs in
 * as, the box key the sign-in is bound to, which is what tells two boxes of one fleet apart, when
 * the person signed in there and when that box last used the sign-in.
 * Revoke ends one: that box trades no token for the viewer again and every access token already
 * minted under it ends with it, so the sessions running on it lose Dispatch and its secrets at
 * once. The viewer's sign-ins on their other machines keep working, and the box signs in again
 * with `agent-secrets login`, which makes a key of its own. Rendered on the Settings page, beside
 * Live grants, because it is where a person manages what they have given, and because it must
 * stay reachable on a deployment whose broker serves no machine-code page at all.
 */
export function SignInsSection(): ReactNode {
  const queryClient = useQueryClient();
  const signIns = useQuery({ ...signInsQuery(), select: (data) => data.signins });

  return (
    <RevocableList
      className="mt-10"
      columns={columns}
      confirm={(signIn) =>
        `Revoke your sign-in on ${signIn.machine}? Every agent session running there loses Dispatch and its secrets at once, and the machine needs you to run \`agent-secrets login\` again.`
      }
      description="Every machine you have signed in on with `agent-secrets login`. Revoking one ends it now: that machine gets nothing more as you, and the sessions running on it lose their access. Your sign-ins on other machines are untouched."
      empty="No sign-ins."
      heading="Sign-ins"
      headingId="signins-heading"
      noun="sign-ins"
      onRevoked={() => void queryClient.invalidateQueries({ queryKey: signInsQuery().queryKey })}
      query={signIns}
      revoke={(signIn, bounds) => api.revokeSignIn(signIn.signin_id, bounds)}
      revokeError="Could not revoke this sign-in."
      rowKey={(signIn) => signIn.signin_id}
    />
  );
}
