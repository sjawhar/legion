import { expect, test } from "bun:test";

import {
  actorAttribution,
  actorLabel,
  actorName,
  sessionLabel,
  shortSessionId,
} from "./actor-label";

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

test("a bare session id is labelled by the title given, else its short id", () => {
  expect(sessionLabel("0123456789abcdef", " Planner ")).toBe("Planner");
  expect(sessionLabel("0123456789abcdef", "   ")).toBe("session:01234567…");
  expect(sessionLabel("0123456789abcdef", undefined)).toBe("session:01234567…");
});

test("the Legion daemon's write identity reads whole, not as a short session id", () => {
  expect(actorLabel({ id: "legion-daemon:INFRA", kind: "session" })).toBe("legion-daemon:INFRA");
  expect(actorLabel({ id: "legion-daemonic-session", kind: "session" })).toBe("session:legion-d…");
});

test("an actor's name never carries the owner or service attribution", () => {
  expect(
    actorName(
      { id: "abcdef1234567890", kind: "session", owner: "alice", origin: { session_title: "x" } },
      titles
    )
  ).toBe("Planner");
  // A blank live title falls back to the stamped one rather than to the short id.
  expect(
    actorName(
      { id: "abcdef1234567890", kind: "session", origin: { session_title: "Stamped" } },
      new Map([["abcdef1234567890", "  "]])
    )
  ).toBe("Stamped");
  expect(actorName({ id: "architecture-importer", kind: "system" })).toBe("architecture-importer");
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

test("an actor's attribution is what its label adds after its name", () => {
  expect(actorAttribution({ id: "s", kind: "session", owner: "alice" })).toBe(" (for alice)");
  // An owner wins over a service: the human the write was made for is the one named.
  expect(
    actorAttribution({
      id: "s",
      kind: "session",
      owner: "alice",
      service: "system:serviceaccount:legion:legion-worker",
    })
  ).toBe(" (for alice)");
  expect(
    actorAttribution({
      id: "s",
      kind: "session",
      service: "system:serviceaccount:legion:legion-worker",
    })
  ).toBe(" (as legion/legion-worker)");
  expect(actorAttribution({ id: "s", kind: "session", origin: { session_title: "x" } })).toBe("");
  expect(actorAttribution({ id: "alice", kind: "user" })).toBe("");
  expect(actorAttribution({ id: "architecture-importer", kind: "system" })).toBe("");
  const owned = { id: "abcdef1234567890", kind: "session" as const, owner: "bob" };
  expect(actorLabel(owned, titles)).toBe(`${actorName(owned, titles)}${actorAttribution(owned)}`);
});
