import { describe, expect, test } from "bun:test";
import { join } from "node:path";
import { scriptFunctions } from "./script-functions";

// assert_gh_files, taken from stage4b-sandbox-tree.sh by name, judges a container's gh bundle as
// `ls -A` lists its root, the repository owner's directory and each entry of that directory inside
// the pod (pod_exec, stubbed here to print ROOT for the root, OWNER for the owner's directory and
// REPO_<name> for the entry <name> under it, defaulting to the path itself, which is what `ls -A`
// prints for a file). The bundle is a whole-volume Secret projection the kubelet rewrites in place
// when the Secret changes, so beside the owners' directories its root always holds the kubelet's
// atomic-writer entries; the owner's own directory holds its two gh files beside a directory per
// other repository the project serves under that owner, each holding those two files alone.
const fn = scriptFunctions(join(import.meta.dir, "..", "stage4b-sandbox-tree.sh"));

function judge(
  root: string,
  owner = "config.yml\nhosts.yml\n",
  repos: Record<string, string> = {}
) {
  const script = [
    "set -Eeuo pipefail",
    'fail() { echo "FAIL: $*"; exit 1; }',
    "gh_owners_dir=/var/run/legion/gh",
    "gh_config_dir=/var/run/legion/gh/sjawhar",
    `pod_exec() {
  local asked="$*" entry held
  entry=\${asked##*/}
  case "$asked" in
    *"$gh_config_dir"/*) held=REPO_$entry; printf "%s" "\${!held-$gh_config_dir/$entry}" ;;
    *"$gh_config_dir"*) printf "%s" "$OWNER" ;;
    *) printf "%s" "$ROOT" ;;
  esac
}`,
    fn("assert_gh_files"),
    'assert_gh_files pod-a controller "the controller container of pod-a"',
    "echo OK",
  ].join("\n");
  const listings = Object.fromEntries(
    Object.entries(repos).map(([name, held]) => [`REPO_${name}`, held])
  );
  const result = Bun.spawnSync(["bash", "-c", script], {
    env: { ...process.env, ROOT: root, OWNER: owner, ...listings },
  });
  return { exitCode: result.exitCode, out: result.stdout.toString() };
}

describe("assert_gh_files", () => {
  test("passes a refreshed projection: the owners' directories beside the kubelet's atomic-writer entries", () => {
    expect(judge("..2026_10_11_13_01_02.123456789\n..data\nacme\nsjawhar\n")).toEqual({
      exitCode: 0,
      out: "OK\n",
    });
  });

  test("fails a bundle without the repository owner's directory, naming it and the listing", () => {
    const { exitCode, out } = judge("..data\nacme\n");
    expect(exitCode).toBe(1);
    expect(out).toBe(
      "FAIL: the controller container of pod-a's /var/run/legion/gh lacks sjawhar; it holds: ..data acme \n"
    );
  });

  test("fails an owner's directory that lacks hosts.yml, naming it and the listing", () => {
    const { exitCode, out } = judge("..data\nsjawhar\n", "config.yml\n");
    expect(exitCode).toBe(1);
    expect(out).toBe(
      "FAIL: the controller container of pod-a's /var/run/legion/gh/sjawhar lacks hosts.yml; it holds: config.yml \n"
    );
  });

  test("fails a root entry that is neither an owner's directory nor the kubelet's, naming it", () => {
    const { exitCode, out } = judge("..data\nsjawhar\nhosts.yml\n");
    expect(exitCode).toBe(1);
    expect(out).toBe(
      "FAIL: the controller container of pod-a's /var/run/legion/gh holds an entry that is neither an owner's directory nor the kubelet's: hosts.yml \n"
    );
  });

  test("fails an owner's directory holding a file beside the two gh files, naming it", () => {
    const { exitCode, out } = judge("..data\nsjawhar\n", "config.yml\nhosts.yml\ntoken\n");
    expect(exitCode).toBe(1);
    expect(out).toBe(
      "FAIL: the controller container of pod-a's /var/run/legion/gh/sjawhar holds an entry that is neither a gh file nor a repository's directory: token\n"
    );
  });

  test("passes an owner's directory holding a repository's directory with both gh files", () => {
    expect(
      judge("..data\nsjawhar\n", "config.yml\nhosts.yml\nlegion\n", {
        legion: "config.yml\nhosts.yml\n",
      })
    ).toEqual({ exitCode: 0, out: "OK\n" });
  });

  test("fails a repository's directory that lacks config.yml, naming it and the listing", () => {
    const { exitCode, out } = judge("..data\nsjawhar\n", "config.yml\nhosts.yml\nlegion\n", {
      legion: "hosts.yml\n",
    });
    expect(exitCode).toBe(1);
    expect(out).toBe(
      "FAIL: the controller container of pod-a's /var/run/legion/gh/sjawhar/legion lacks config.yml; it holds: hosts.yml \n"
    );
  });

  test("fails a directory of the owner's named as no repository is, lowercased, with both gh files in it", () => {
    const { exitCode, out } = judge("..data\nsjawhar\n", "config.yml\nhosts.yml\nLegion\n", {
      Legion: "config.yml\nhosts.yml\n",
    });
    expect(exitCode).toBe(1);
    expect(out).toBe(
      "FAIL: the controller container of pod-a's /var/run/legion/gh/sjawhar holds an entry that is neither a gh file nor a repository's directory: Legion\n"
    );
  });

  test("fails a bundle without the kubelet's ..data, which a subPath mount would leave and never refresh", () => {
    const { exitCode, out } = judge("sjawhar\n");
    expect(exitCode).toBe(1);
    expect(out).toBe(
      "FAIL: the controller container of pod-a's /var/run/legion/gh holds no ..data: it is not the whole-volume projection the kubelet refreshes in place; it holds: sjawhar \n"
    );
  });
});
