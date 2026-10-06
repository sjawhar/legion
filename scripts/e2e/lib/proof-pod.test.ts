import { afterAll, describe, expect, test } from "bun:test";
import {
  chmodSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  statSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

// lib/proof-pod.sh outside a pod: a fake ServiceAccount directory stands for the pod's mounted
// token, and a fake kubectl first on PATH logs its argv and answers a TokenRequest. Nothing here
// reaches a cluster or GitHub; the refusals each come before any network call.
const script = join(import.meta.dir, "proof-pod.sh");
const dir = mkdtempSync(join(tmpdir(), "proof-pod-test."));
afterAll(() => rmSync(dir, { recursive: true, force: true }));

const bin = join(dir, "bin");
mkdirSync(bin);
writeFileSync(
  join(bin, "kubectl"),
  `#!/usr/bin/env bash
printf '%s\\n' "$*" >>"$FAKE_KUBECTL_LOG"
[ "\${FAKE_KUBECTL_EXIT:-0}" = 0 ] || { echo "fake kubectl: forbidden" >&2; exit "$FAKE_KUBECTL_EXIT"; }
printf '{"kind":"TokenRequest","status":{"token":"fake-token","expirationTimestamp":"2026-10-07T00:00:00Z"}}\\n'
`,
  { mode: 0o755 }
);
const serviceAccount = join(dir, "serviceaccount");
mkdirSync(serviceAccount);
writeFileSync(join(serviceAccount, "token"), "operator-token\n");
writeFileSync(join(serviceAccount, "ca.crt"), "ca\n");

let runs = 0;
function run(args: string[], env: Record<string, string> = {}, stdin = "") {
  const log = join(dir, `kubectl-${++runs}.log`);
  writeFileSync(log, "");
  const result = Bun.spawnSync(["bash", script, ...args], {
    env: {
      PATH: `${bin}:/usr/bin:/bin`,
      HOME: dir,
      TMPDIR: dir,
      FAKE_KUBECTL_LOG: log,
      KUBERNETES_SERVICE_HOST: "10.0.0.1",
      KUBERNETES_SERVICE_PORT: "443",
      LEGION_PROOF_SERVICE_ACCOUNT_DIR: serviceAccount,
      LEGION_PROOF_DAEMON_SERVICE_ACCOUNT: "legion-proof/legion-proof-daemon",
      ...env,
    },
    stdin: Buffer.from(stdin),
  });
  return {
    status: result.exitCode,
    stdout: result.stdout.toString(),
    stderr: result.stderr.toString(),
    kubectl: readFileSync(log, "utf8").split("\n").filter(Boolean),
  };
}

describe("kubeconfig", () => {
  test("writes an operator context on the pod's token and a runtime context minting the daemon's", () => {
    const file = join(dir, "kubeconfig");
    const r = run(["kubeconfig", file]);
    expect(r.status).toBe(0);
    expect(statSync(file).mode & 0o777).toBe(0o600);
    const config = JSON.parse(readFileSync(file, "utf8"));
    expect(config.clusters).toEqual([
      {
        name: "pod",
        cluster: {
          server: "https://10.0.0.1:443",
          "certificate-authority": join(serviceAccount, "ca.crt"),
        },
      },
    ]);
    const users = Object.fromEntries(
      config.users.map((u: { name: string; user: unknown }) => [u.name, u.user])
    );
    expect(users.operator).toEqual({ tokenFile: join(serviceAccount, "token") });
    expect(users.runtime.exec.apiVersion).toBe("client.authentication.k8s.io/v1");
    expect(users.runtime.exec.interactiveMode).toBe("Never");
    expect(users.runtime.exec.args).toEqual([
      script,
      "runtime-token",
      file,
      "legion-proof",
      "legion-proof-daemon",
    ]);
    expect(
      config.contexts.map((c: { name: string; context: { user: string } }) => [
        c.name,
        c.context.user,
      ])
    ).toEqual([
      ["operator", "operator"],
      ["runtime", "runtime"],
    ]);
  });

  test("brackets an IPv6 API server address", () => {
    const file = join(dir, "kubeconfig-v6");
    expect(run(["kubeconfig", file], { KUBERNETES_SERVICE_HOST: "fd00::1" }).status).toBe(0);
    expect(JSON.parse(readFileSync(file, "utf8")).clusters[0].cluster.server).toBe(
      "https://[fd00::1]:443"
    );
  });

  test("refuses without the daemon's ServiceAccount, naming the input", () => {
    const r = run(["kubeconfig", join(dir, "none")], { LEGION_PROOF_DAEMON_SERVICE_ACCOUNT: "" });
    expect(r.status).toBe(1);
    expect(r.stderr).toContain("LEGION_PROOF_DAEMON_SERVICE_ACCOUNT is unset");
  });

  test("refuses a ServiceAccount that is not namespace/name", () => {
    const r = run(["kubeconfig", join(dir, "none")], {
      LEGION_PROOF_DAEMON_SERVICE_ACCOUNT: "legion-proof-daemon",
    });
    expect(r.status).toBe(1);
    expect(r.stderr).toContain("is not <namespace>/<name>");
  });

  test("refuses outside a pod", () => {
    const r = run(["kubeconfig", join(dir, "none")], { KUBERNETES_SERVICE_HOST: "" });
    expect(r.status).toBe(1);
    expect(r.stderr).toContain("KUBERNETES_SERVICE_HOST and KUBERNETES_SERVICE_PORT are unset");
  });

  test("refuses a pod with no ServiceAccount token mounted", () => {
    const r = run(["kubeconfig", join(dir, "none")], {
      LEGION_PROOF_SERVICE_ACCOUNT_DIR: join(dir, "no-such-dir"),
    });
    expect(r.status).toBe(1);
    expect(r.stderr).toContain("holds no readable token and ca.crt");
  });
});

describe("runtime-token", () => {
  test("asks the TokenRequest API as the operator and answers an ExecCredential with its expiry", () => {
    const r = run(["runtime-token", "/k/config", "legion-proof", "legion-proof-daemon"]);
    expect(r.status).toBe(0);
    expect(r.kubectl).toEqual([
      "--kubeconfig /k/config --context operator -n legion-proof create token legion-proof-daemon --duration 1h -o json",
    ]);
    expect(JSON.parse(r.stdout)).toEqual({
      apiVersion: "client.authentication.k8s.io/v1",
      kind: "ExecCredential",
      status: { token: "fake-token", expirationTimestamp: "2026-10-07T00:00:00Z" },
    });
  });

  test("a refused TokenRequest fails with kubectl's reason and no credential", () => {
    const r = run(["runtime-token", "/k/config", "legion-proof", "default"], {
      FAKE_KUBECTL_EXIT: "1",
    });
    expect(r.status).toBe(1);
    expect(r.stdout).toBe("");
    expect(r.stderr).toContain("fake kubectl: forbidden");
    expect(r.stderr).toContain("could not mint a token of ServiceAccount legion-proof/default");
  });
});

describe("run", () => {
  const stage = join(dir, "stage.sh");
  writeFileSync(
    stage,
    `printf '%s=%s\\n' KUBECONFIG "$KUBECONFIG" RUNTIME_KUBECONFIG "$LEGION_E2E_RUNTIME_KUBECONFIG" \\
  RUNTIME_CONTEXT "$LEGION_E2E_RUNTIME_CONTEXT" OPERATOR_CONTEXT "$LEGION_E2E_OPERATOR_CONTEXT" \\
  RUNTIME_SERVICE_ACCOUNT "$LEGION_E2E_RUNTIME_SERVICE_ACCOUNT" ARGS "$*"
`
  );

  test("runs the stage with the kubeconfig and the identity inputs it answers", () => {
    const r = run(["run", stage, "one", "two"]);
    expect(r.status).toBe(0);
    const env = Object.fromEntries(
      r.stdout
        .trim()
        .split("\n")
        .map((line) => line.split(/=(.*)/s).slice(0, 2))
    );
    expect(env.KUBECONFIG).toBe(env.RUNTIME_KUBECONFIG);
    expect(env.KUBECONFIG).toStartWith(join(dir, "legion-proof-pod."));
    expect(JSON.parse(readFileSync(env.KUBECONFIG, "utf8")).kind).toBe("Config");
    expect(env.RUNTIME_CONTEXT).toBe("runtime");
    expect(env.OPERATOR_CONTEXT).toBe("operator");
    expect(env.RUNTIME_SERVICE_ACCOUNT).toBe("legion-proof/legion-proof-daemon");
    expect(env.ARGS).toBe("one two");
  });

  for (const name of [
    "KUBECONFIG",
    "LEGION_E2E_RUNTIME_KUBECONFIG",
    "LEGION_E2E_RUNTIME_CONTEXT",
    "LEGION_E2E_OPERATOR_CONTEXT",
    "LEGION_E2E_RUNTIME_SERVICE_ACCOUNT",
  ]) {
    test(`refuses to override ${name} the caller set`, () => {
      const r = run(["run", stage], { [name]: "callers" });
      expect(r.status).toBe(1);
      expect(r.stdout).toBe("");
      expect(r.stderr).toContain(`${name} is set`);
    });
  }
});

describe("app-token and git-credential refusals", () => {
  const pem = "-----BEGIN RSA PRIVATE KEY-----\nnot a key\n-----END RSA PRIVATE KEY-----\n";
  // Each mode set after the write, since the umask narrows the one writeFileSync is given.
  const keyFile = (name: string, content: string, mode: number) => {
    const path = join(dir, name);
    writeFileSync(path, content);
    chmodSync(path, mode);
    return path;
  };
  const key = keyFile("app.pem", pem, 0o600);
  const loose = keyFile("loose.pem", pem, 0o640);
  const notPem = keyFile("not.pem", "LS0tLS1CRUdJTg==\n", 0o600);

  test("refuses a key file its group or others can read, naming the mode", () => {
    const r = run(["app-token", "1", loose, "acme/widgets"]);
    expect(r.status).toBe(1);
    expect(r.stdout).toBe("");
    expect(r.stderr).toContain("lets its group or others in (mode 640)");
  });

  test("refuses a key file that holds no PEM block", () => {
    const r = run(["app-token", "1", notPem, "acme/widgets"]);
    expect(r.status).toBe(1);
    expect(r.stderr).toContain("holds no PEM block");
  });

  test("refuses an App id that is not a number and a repository that is not owner/name", () => {
    expect(run(["app-token", "x1", key, "acme/widgets"]).stderr).toContain("is not a number");
    expect(run(["app-token", "1", key, "widgets"]).stderr).toContain("is not <owner>/<name>");
  });

  test("git-credential answers nothing for another host, or for store and erase", () => {
    const other = run(
      ["git-credential", "1", key, "acme/widgets", "get"],
      {},
      "protocol=https\nhost=example.com\n\n"
    );
    expect(other.status).toBe(0);
    expect(other.stdout).toBe("");
    for (const operation of ["store", "erase"]) {
      const r = run(
        ["git-credential", "1", key, "acme/widgets", operation],
        {},
        "protocol=https\nhost=github.com\n\n"
      );
      expect(r.status).toBe(0);
      expect(r.stdout).toBe("");
    }
  });

  test("human refuses a gh that is its own directory's", () => {
    const human = join(dir, "human");
    mkdirSync(human);
    writeFileSync(join(human, "gh"), "#!/bin/sh\n", { mode: 0o700 });
    const r = run(["human", human, "1", key, "acme/widgets"], {
      PATH: `${human}:${bin}:/usr/bin:/bin`,
    });
    expect(r.status).toBe(1);
    expect(r.stderr).toContain("this directory's own");
  });
});
