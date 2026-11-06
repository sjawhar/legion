import { expect, test } from "bun:test";
import { join } from "node:path";

// The gate the three cluster proofs read LEGION_E2E_POD_SERVICE_ACCOUNT through: stdout is the
// ServiceAccount a run puts in place of the operator route's ${POD_SERVICE_ACCOUNT} placeholder,
// and a refusal names the variable on stderr, never its value, and prints nothing on stdout.
const script = join(import.meta.dir, "pod-service-account.sh");

function run(account?: string) {
  const env: Record<string, string> = { PATH: process.env.PATH ?? "" };
  if (account !== undefined) env.LEGION_E2E_POD_SERVICE_ACCOUNT = account;
  const result = Bun.spawnSync(["bash", script], { env });
  return {
    code: result.exitCode,
    stdout: result.stdout.toString(),
    stderr: result.stderr.toString(),
  };
}

test("the variable unset is refused by name, with nothing on stdout", () => {
  const { code, stdout, stderr } = run();
  expect(code).toBe(1);
  expect(stdout).toBe("");
  expect(stderr).toContain("LEGION_E2E_POD_SERVICE_ACCOUNT is unset");
});

test("the variable empty is refused the same way", () => {
  const { code, stdout, stderr } = run("");
  expect(code).toBe(1);
  expect(stdout).toBe("");
  expect(stderr).toContain("LEGION_E2E_POD_SERVICE_ACCOUNT is unset");
});

// A ServiceAccount name is a DNS-1123 subdomain: lowercase letters, digits, - and ., beginning and
// ending with a letter or a digit, at most 253 characters. Each refusal names the variable and
// never the value, which names the operator's own infrastructure.
test.each([
  ["an upper-case letter", "Operator-account"],
  ["a space", "operator account"],
  ["a leading -", "-operator-account"],
  ["a trailing .", "operator-account."],
  ["254 characters", `${"a".repeat(253)}b`],
])("%s is refused, naming the variable and not the value", (_what, account) => {
  const { code, stdout, stderr } = run(account);
  expect(code).toBe(1);
  expect(stdout).toBe("");
  expect(stderr).toContain(
    "LEGION_E2E_POD_SERVICE_ACCOUNT is not a Kubernetes ServiceAccount name"
  );
  expect(stderr).not.toContain(account);
});

test.each([
  ["a name", "operator-account"],
  ["a name with a dot", "operator.account"],
  ["253 characters", "a".repeat(253)],
])("%s is printed unchanged", (_what, account) => {
  const { code, stdout, stderr } = run(account);
  expect(stderr).toBe("");
  expect(code).toBe(0);
  expect(stdout).toBe(`${account}\n`);
});
