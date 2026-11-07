import { expect, test } from "bun:test";

import type { Artifact, GatedStep, IssueDetails, Project } from "../../api/types";
import { issue as fixture, specArtifact } from "../margin/margin-fixture";
import { gatedStepEntries } from "./gated-steps";

const decisionsDoc: Artifact = {
  ...specArtifact,
  id: "artifact-2",
  name: "decisions.md",
  slug: "decisions",
  primary: false,
  role: "decisions",
};

const verificationDoc: Artifact = {
  ...specArtifact,
  id: "artifact-3",
  name: "verification.md",
  slug: "verification",
  primary: false,
  role: "verification",
};

/** A root issue on a Dispatch older than the design gate's steps: it answers no decisions
 *  document id at all, the way an older server leaves the field off the wire. */
const olderRoot: IssueDetails = {
  ...fixture,
  artifacts: [specArtifact, decisionsDoc, verificationDoc],
  verification_artifact_id: "artifact-3",
};

const root: IssueDetails = { ...olderRoot, decisions_artifact_id: "artifact-2" };

const ungated: Project = { key: "CORE", name: "Core", created_at: "2026-09-09T00:00:00Z" };

function gating(...gated_steps: GatedStep[]): Project {
  return { ...ungated, gated_steps };
}

test("a project whose deployment published no gated steps draws none", () => {
  expect(gatedStepEntries(ungated, root)).toEqual([]);
});

test("a project that gates nothing draws none", () => {
  expect(gatedStepEntries(gating(), root)).toEqual([]);
});

test("an issue that is not its tree's root draws none, whatever its project gates", () => {
  const child: IssueDetails = { ...root, parent: "CORE-9" };

  expect(gatedStepEntries(gating("decisions", "spec"), child)).toEqual([]);
});

test("neither the unread project nor the unread issue draws a step", () => {
  expect(gatedStepEntries(undefined, root)).toEqual([]);
  expect(gatedStepEntries(gating("spec"), undefined)).toEqual([]);
});

test("the steps are drawn in the order the deployment published them, not the enumeration's", () => {
  expect(gatedStepEntries(gating("spec", "decisions"), root)).toEqual([
    { step: "spec", label: "Spec", document: specArtifact },
    { step: "decisions", label: "Decisions", document: decisionsDoc },
  ]);
});

test("each step names its own document, the Verification document among them", () => {
  expect(gatedStepEntries(gating("decisions", "criteria", "spec"), root)).toEqual([
    { step: "decisions", label: "Decisions", document: decisionsDoc },
    { step: "criteria", label: "Verification", document: verificationDoc },
    { step: "spec", label: "Spec", document: specArtifact },
  ]);
});

test("a step the issue holds no document for is drawn with no document", () => {
  const none: IssueDetails = { ...root, decisions_artifact_id: "" };

  expect(gatedStepEntries(gating("decisions"), none)).toEqual([
    { step: "decisions", label: "Decisions", document: undefined },
  ]);
});

test("a step an older server answers no id for at all is drawn with no document", () => {
  expect(gatedStepEntries(gating("decisions"), olderRoot)).toEqual([
    { step: "decisions", label: "Decisions", document: undefined },
  ]);
});

test("a step word this build has no name for is left out and the rest keep their order", () => {
  // A Dispatch newer than this build may gate a fourth step; the header draws what it knows.
  const newer: Project = {
    ...ungated,
    gated_steps: ["spec", "thresholds", "decisions"] as unknown as GatedStep[],
  };

  expect(gatedStepEntries(newer, root)).toEqual([
    { step: "spec", label: "Spec", document: specArtifact },
    { step: "decisions", label: "Decisions", document: decisionsDoc },
  ]);
});
