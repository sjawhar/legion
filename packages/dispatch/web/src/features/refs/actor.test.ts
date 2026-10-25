import { expect, test } from "bun:test";

import type { AskResolution } from "../../api/types";
import {
  actorLabel,
  askResolutionBadge,
  describeAskResolution,
  describeAskResolutionActor,
  shortSessionId,
} from "./actor";

const titles = new Map([["abcdef1234567890", "Planner"]]);

test("a session is labelled by its live registry title, else its stamped title, else its short id", () => {
  expect(
    actorLabel(
      { id: "abcdef1234567890", kind: "session", origin: { session_title: "old title" } },
      titles
    )
  ).toBe("Planner");
  expect(
    actorLabel({ id: "zzz", kind: "session", origin: { session_title: "Fix Margin Log" } }, titles)
  ).toBe("Fix Margin Log");
  expect(actorLabel({ id: "0123456789abcdef", kind: "session" }, titles)).toBe("session:01234567…");
  expect(
    actorLabel({ id: "0123456789abcdef", kind: "session", origin: { session_title: "  " } })
  ).toBe(shortSessionId("0123456789abcdef"));
  expect(actorLabel({ id: "alice", kind: "user" }, titles)).toBe("alice");
});

test("the Legion daemon's write identity reads whole, not as a short session id", () => {
  expect(actorLabel({ id: "legion-daemon:INFRA", kind: "session" })).toBe("legion-daemon:INFRA");
  expect(actorLabel({ id: "legion-daemonic-session", kind: "session" })).toBe("session:legion-d…");
});

test("a personally attributed session keeps its human owner after the same label", () => {
  expect(
    actorLabel({
      id: "architect-session",
      kind: "session",
      origin: { session_title: "Architect" },
      owner: "alice",
    })
  ).toBe("Architect (for alice)");
  expect(
    actorLabel(
      {
        id: "abcdef1234567890",
        kind: "session",
        origin: { session_title: "old title" },
        owner: "alice",
      },
      titles
    )
  ).toBe("Planner (for alice)");
  expect(actorLabel({ id: "0123456789abcdef", kind: "session", owner: "bob" })).toBe(
    "session:01234567… (for bob)"
  );
});

test("a session a service token authenticated keeps the service account after the label", () => {
  expect(
    actorLabel({
      id: "worker-session",
      kind: "session",
      origin: { session_title: "Implementer" },
      service: "system:serviceaccount:legion:legion-worker",
    })
  ).toBe("Implementer (as legion/legion-worker)");
  expect(
    actorLabel(
      { id: "abcdef1234567890", kind: "session", service: "system:serviceaccount:legion:dispatch" },
      titles
    )
  ).toBe("Planner (as legion/dispatch)");
  // Two namespaces' `default` are different identities, so they must not read alike.
  expect(
    actorLabel({
      id: "other-session",
      kind: "session",
      service: "system:serviceaccount:kube-system:default",
    })
  ).toBe("session:other-se… (as kube-system/default)");
  // Not a Kubernetes subject: shown whole rather than guessed at.
  expect(actorLabel({ id: "0123456789abcdef", kind: "session", service: "legion-worker" })).toBe(
    "session:01234567… (as legion-worker)"
  );
});

test("a withdrawal names the closer once and drops the server's reason, which only repeats it", () => {
  const at = "2026-10-25T00:00:00Z";
  const withdrawn = (actor: AskResolution["actor"]): AskResolution => ({
    actor,
    at,
    kind: "withdrawn",
    reason: "Withdrawn by the close of CORE-1.",
  });
  expect(describeAskResolution(withdrawn({ id: "alice", kind: "user" }))).toBe(
    "Withdrawn when alice closed the issue"
  );
  expect(describeAskResolutionActor(withdrawn({ id: "legion-daemon:CORE", kind: "session" }))).toBe(
    "Withdrawn when legion-daemon:CORE closed the issue"
  );
  expect(askResolutionBadge(withdrawn({ id: "alice", kind: "user" }))).toBe("Withdrawn");

  const retracted: AskResolution = {
    actor: { id: "0123456789abcdef", kind: "session" },
    at,
    kind: "retracted",
    reason: "Duplicate.",
  };
  expect(describeAskResolution(retracted)).toBe("Retracted by session:01234567… - Duplicate.");
  expect(askResolutionBadge(retracted)).toBe("Retracted");
  const resolved: AskResolution = { ...retracted, kind: "resolved", reason: "Done." };
  expect(describeAskResolution(resolved)).toBe("Resolved by session:01234567… - Done.");
  expect(askResolutionBadge(resolved)).toBe("Resolved");
});
