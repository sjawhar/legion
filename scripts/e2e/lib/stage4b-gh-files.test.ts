import { describe, expect, test } from "bun:test";
import { join } from "node:path";
import { scriptFunctions } from "./script-functions";

// assert_gh_files, taken from stage4b-sandbox-tree.sh by name, judges a container's gh config
// directory as `ls -A` lists it inside the pod (pod_exec, stubbed here to print LISTING). The
// directory is a whole-volume Secret projection the kubelet rewrites in place when the Secret
// changes, so beside the two gh files it always holds the kubelet's atomic-writer entries.
const fn = scriptFunctions(join(import.meta.dir, "..", "stage4b-sandbox-tree.sh"));

function judge(listing: string) {
  const script = [
    "set -Eeuo pipefail",
    'fail() { echo "FAIL: $*"; exit 1; }',
    'pod_exec() { printf "%s" "$LISTING"; }',
    "gh_config_dir=/var/run/legion/gh",
    fn("assert_gh_files"),
    'assert_gh_files pod-a controller "the controller container of pod-a"',
    "echo OK",
  ].join("\n");
  const result = Bun.spawnSync(["bash", "-c", script], {
    env: { ...process.env, LISTING: listing },
  });
  return { exitCode: result.exitCode, out: result.stdout.toString() };
}

describe("assert_gh_files", () => {
  test("passes a refreshed projection: both gh files beside the kubelet's atomic-writer entries", () => {
    expect(judge("..2026_10_11_13_01_02.123456789\n..data\nconfig.yml\nhosts.yml\n")).toEqual({
      exitCode: 0,
      out: "OK\n",
    });
  });

  test("fails a directory that lacks hosts.yml, naming it and the listing", () => {
    const { exitCode, out } = judge("..data\nconfig.yml\n");
    expect(exitCode).toBe(1);
    expect(out).toBe(
      "FAIL: the controller container of pod-a's /var/run/legion/gh lacks hosts.yml; it holds: ..data config.yml \n"
    );
  });

  test("fails an entry that is neither a gh file nor the kubelet's, naming it", () => {
    const { exitCode, out } = judge("..data\nconfig.yml\nhosts.yml\ntoken\n");
    expect(exitCode).toBe(1);
    expect(out).toBe(
      "FAIL: the controller container of pod-a's /var/run/legion/gh holds an entry that is neither a gh file nor the kubelet's: token \n"
    );
  });

  test("fails a directory without the kubelet's ..data, which a subPath mount would leave and never refresh", () => {
    const { exitCode, out } = judge("config.yml\nhosts.yml\n");
    expect(exitCode).toBe(1);
    expect(out).toBe(
      "FAIL: the controller container of pod-a's /var/run/legion/gh holds no ..data: it is not the whole-volume projection the kubelet refreshes in place; it holds: config.yml hosts.yml \n"
    );
  });
});
