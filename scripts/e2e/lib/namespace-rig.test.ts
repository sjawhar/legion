import { describe, expect, test } from "bun:test";
import { mkdtempSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

// namespace-clean holds the run to its own objects: those labelled with its project, and those
// with no project label whose name the run's daemon or harness made (from the label, or from it
// as a claim token carries it). The rig is sourced as a stage proof sources it; `op` is stubbed to
// answer the two listings the snapshot makes, the unlabelled one and the run's labelled one, from
// UNLABELLED and LABELLED.
const rig = join(import.meta.dir, "namespace-rig.sh");

interface Listing {
  unlabelled: string[];
  labelled: string[];
}

function clean(label: string, before: Listing, after: Listing, failListing = false) {
  const evidence = mkdtempSync(join(tmpdir(), "namespace-rig-"));
  const script = [
    "set -Eeuo pipefail",
    `source ${JSON.stringify(rig)}`,
    `run_label=${JSON.stringify(label)} namespace=legion evidence=${JSON.stringify(evidence)} compared= check=snapshot FAIL_LISTING=`,
    'begin() { check=$1; echo "== $1"; }',
    'note() { echo "   $*"; }',
    'pass() { echo "CHECK $check: PASS"; }',
    'fail() { echo "CHECK $check: FAIL: $*"; exit 1; }',
    'op() { [ "$FAIL_LISTING" != 1 ] || return 1; case $4 in "!legion.dev/project") printf "%s" "$UNLABELLED" ;; *) printf "%s" "$LABELLED" ;; esac; }',
    "UNLABELLED=$BEFORE_UNLABELLED LABELLED=$BEFORE_LABELLED",
    'snapshot "$evidence/namespace-before.txt" || fail "the operator could not list namespace $namespace"',
    "UNLABELLED=$AFTER_UNLABELLED LABELLED=$AFTER_LABELLED",
    `FAIL_LISTING=${failListing ? "1" : ""}`,
    "namespace_clean",
  ].join("\n");
  const result = Bun.spawnSync(["bash", "-c", script], {
    env: {
      ...process.env,
      BEFORE_UNLABELLED: lines(before.unlabelled),
      BEFORE_LABELLED: lines(before.labelled),
      AFTER_UNLABELLED: lines(after.unlabelled),
      AFTER_LABELLED: lines(after.labelled),
    },
  });
  return { exitCode: result.exitCode, out: result.stdout.toString() + result.stderr.toString() };
}

function lines(names: string[]) {
  return names.map((name) => `${name}\n`).join("");
}

// The deployment's own objects, present before and after every run.
const shared = [
  "configmap/kube-root-ca.crt",
  "configmap/legion-operator-route",
  "configmap/legion-worker-aws-config",
];

describe("namespace_clean", () => {
  test("passes when another system creates an unlabelled object of its own during the run", () => {
    const { exitCode, out } = clean(
      "legsmoke",
      { unlabelled: shared, labelled: [] },
      { unlabelled: [...shared, "configmap/legion-worker-kubeconfig"], labelled: [] }
    );
    expect(out).toContain("CHECK namespace-clean: PASS");
    expect(exitCode).toBe(0);
  });

  test("passes when another system removes an unlabelled object of its own during the run", () => {
    const { exitCode, out } = clean(
      "legsmoke",
      { unlabelled: shared, labelled: [] },
      { unlabelled: shared.slice(1), labelled: [] }
    );
    expect(out).toContain("CHECK namespace-clean: PASS");
    expect(exitCode).toBe(0);
  });

  test("fails on an object labelled with the run's project left after the teardown", () => {
    const { exitCode, out } = clean(
      "legsmoke",
      { unlabelled: shared, labelled: [] },
      { unlabelled: shared, labelled: ["sandbox.agents.x-k8s.io/legion-legsmoke-legsmoke-101"] }
    );
    expect(exitCode).toBe(1);
    expect(out).toContain("+sandbox.agents.x-k8s.io/legion-legsmoke-legsmoke-101");
    expect(out).toContain(
      "CHECK namespace-clean: FAIL: namespace legion differs from its snapshot"
    );
  });

  test("fails on an unlabelled object named for the run, which the teardown's label selector never deletes", () => {
    const { exitCode, out } = clean(
      "legsmoke",
      { unlabelled: shared, labelled: [] },
      {
        unlabelled: [...shared, "secret/legion-legsmoke-legsmoke-101-implementer-boot"],
        labelled: [],
      }
    );
    expect(exitCode).toBe(1);
    expect(out).toContain("+secret/legion-legsmoke-legsmoke-101-implementer-boot");
  });

  test("names a run object by its label as written or as a claim token carries it", () => {
    for (const name of [
      "configmap/legion-operator-route-s4a-20261011120000-ab12",
      "secret/legion-s4a20261011120000ab12-s4a20261011120000ab12-1-boot",
    ]) {
      const { exitCode, out } = clean(
        "s4a-20261011120000-ab12",
        { unlabelled: shared, labelled: [] },
        { unlabelled: [...shared, name], labelled: [] }
      );
      expect(exitCode).toBe(1);
      expect(out).toContain(`+${name}`);
    }
  });

  test("fails, naming the namespace, when the operator cannot list it", () => {
    const { exitCode, out } = clean(
      "legsmoke",
      { unlabelled: shared, labelled: [] },
      { unlabelled: shared, labelled: [] },
      true
    );
    expect(exitCode).toBe(1);
    expect(out).toContain(
      "CHECK namespace-clean: FAIL: the operator could not list namespace legion"
    );
  });
});
