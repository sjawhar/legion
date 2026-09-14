import { describe, expect, it } from "bun:test";
import { mkdtemp, rm, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import {
  createK8sClient,
  IN_CLUSTER_SERVICE_ACCOUNT_DIR,
  K8sApiError,
  type K8sPod,
  resolveK8sCredentials,
} from "../k8s-client";
import { createFakeK8sApi, type FakeK8sEvent } from "./fake-k8s-api";

const NOW = Date.parse("2026-09-13T00:00:00.000Z");

function pod(name: string, labels?: Record<string, string>): K8sPod {
  return { metadata: { name, labels }, spec: {} };
}

async function tempDir(prefix: string): Promise<string> {
  return await mkdtemp(path.join(os.tmpdir(), prefix));
}

describe("createK8sClient / pods", () => {
  it("creates, gets, and lists pods by label selector, encoding the query", async () => {
    const api = createFakeK8sApi({ namespace: "legion", now: () => NOW });
    const client = createK8sClient({
      server: "https://fake.test",
      namespace: "legion",
      fetch: api.fetch,
    });

    const created = await client.pods.create(pod("legion-x-planner-g1", { role: "planner" }));
    expect(created.metadata.uid).toBeTruthy();
    expect(created.metadata.creationTimestamp).toBe(new Date(NOW).toISOString());
    expect(created.status?.phase).toBe("Pending");

    const fetched = await client.pods.get("legion-x-planner-g1");
    expect(fetched.metadata.name).toBe("legion-x-planner-g1");

    await client.pods.create(pod("legion-y-implementer-g1", { role: "implementer" }));
    const list = await client.pods.list("role=planner");
    expect(list.map((item) => item.metadata.name)).toEqual(["legion-x-planner-g1"]);

    const listRequest = api.requests.find((request) => request.path.startsWith("/pods?"));
    expect(listRequest?.path).toBe("/pods?labelSelector=role%3Dplanner");
  });

  it("delete sends gracePeriodSeconds and preconditions.uid; a uid mismatch surfaces as a 409", async () => {
    const api = createFakeK8sApi({ namespace: "legion", now: () => NOW });
    const client = createK8sClient({
      server: "https://fake.test",
      namespace: "legion",
      fetch: api.fetch,
    });
    const created = await client.pods.create(pod("x"));

    await expect(
      client.pods.delete("x", { gracePeriodSeconds: 30, uid: "wrong-uid" })
    ).rejects.toMatchObject({ status: 409, verb: "delete", resource: "pods/x" });

    const deleteRequest = api.requests.find((request) => request.method === "DELETE");
    expect(deleteRequest?.body).toEqual({
      apiVersion: "v1",
      kind: "DeleteOptions",
      gracePeriodSeconds: 30,
      preconditions: { uid: "wrong-uid" },
    });

    await client.pods.delete("x", { gracePeriodSeconds: 30, uid: created.metadata.uid });
    expect(api.pods.get("x")?.metadata.deletionTimestamp).toBeTruthy();
  });

  it("404 on a missing pod surfaces as K8sApiError", async () => {
    const api = createFakeK8sApi({ namespace: "legion", now: () => NOW });
    const client = createK8sClient({
      server: "https://fake.test",
      namespace: "legion",
      fetch: api.fetch,
    });
    await expect(client.pods.get("missing")).rejects.toMatchObject({ status: 404 });
  });

  it("a 403 names the verb and resource in the error message", async () => {
    const api = createFakeK8sApi({
      namespace: "legion",
      now: () => NOW,
      forbidden: ["delete pods"],
    });
    const client = createK8sClient({
      server: "https://fake.test",
      namespace: "legion",
      fetch: api.fetch,
    });
    await client.pods.create(pod("x"));

    try {
      await client.pods.delete("x", { gracePeriodSeconds: 0 });
      throw new Error("expected delete to be forbidden");
    } catch (error) {
      expect(error).toBeInstanceOf(K8sApiError);
      const apiError = error as K8sApiError;
      expect(apiError.status).toBe(403);
      expect(apiError.message).toContain("delete");
      expect(apiError.message).toContain("pods/x");
    }
  });

  it("log sends container and tailLines with the wildcard Accept the apiserver negotiates, and returns the stored text", async () => {
    const api = createFakeK8sApi({ namespace: "legion", now: () => NOW });
    api.logs.set("legion-x-planner-g1/workspace-init", "hello from init\n");
    let capturedAccept: string | null | undefined;
    const recordingFetch = (async (input: string | URL | Request, init?: RequestInit) => {
      capturedAccept = new Headers(init?.headers).get("accept");
      return await api.fetch(input, init);
    }) as typeof fetch;
    const client = createK8sClient({
      server: "https://fake.test",
      namespace: "legion",
      fetch: recordingFetch,
    });

    const text = await client.pods.log("legion-x-planner-g1", "workspace-init", 200);
    expect(text).toBe("hello from init\n");
    // The real apiserver (kind v1.31.0) answers a bare `text/plain` Accept on the log subresource
    // with 406 -- it negotiates against json/yaml/protobuf before its text streamer runs -- and
    // kubectl itself sends `application/json, */*`; the wildcard is what carries the request.
    expect(capturedAccept).toBe("*/*");

    const logRequest = api.requests.find((request) => request.path.includes("/log"));
    expect(logRequest?.path).toBe(
      "/pods/legion-x-planner-g1/log?container=workspace-init&tailLines=200"
    );
  });
});

describe("createK8sClient / persistentVolumeClaims", () => {
  it("patchAnnotations sends a merge patch and a null value removes the annotation", async () => {
    const api = createFakeK8sApi({ namespace: "legion", now: () => NOW });
    const client = createK8sClient({
      server: "https://fake.test",
      namespace: "legion",
      fetch: api.fetch,
    });
    await client.persistentVolumeClaims.create({
      metadata: {
        name: "tree-1",
        annotations: { "legion.dev/unreferenced-since": "2026-01-01T00:00:00.000Z" },
      },
      spec: {},
    });

    let capturedContentType: string | null | undefined;
    const recordingFetch = (async (input: string | URL | Request, init?: RequestInit) => {
      capturedContentType =
        init?.headers instanceof Headers
          ? init.headers.get("Content-Type")
          : (init?.headers as Record<string, string> | undefined)?.["Content-Type"];
      return await api.fetch(input, init);
    }) as typeof fetch;
    const recordingClient = createK8sClient({
      server: "https://fake.test",
      namespace: "legion",
      fetch: recordingFetch,
    });

    await recordingClient.persistentVolumeClaims.patchAnnotations("tree-1", {
      "legion.dev/unreferenced-since": null,
      "legion.dev/new": "value",
    });

    expect(capturedContentType).toBe("application/merge-patch+json");
    const patchRequest = api.requests.find((request) => request.method === "PATCH");
    expect(patchRequest?.body).toEqual({
      metadata: {
        annotations: { "legion.dev/unreferenced-since": null, "legion.dev/new": "value" },
      },
    });
    expect(api.pvcs.get("tree-1")?.metadata.annotations).toEqual({ "legion.dev/new": "value" });
  });

  it("without a storageClass, PVC create/get round-trip carries no extra fields", async () => {
    const api = createFakeK8sApi({ namespace: "legion", now: () => NOW });
    const client = createK8sClient({
      server: "https://fake.test",
      namespace: "legion",
      fetch: api.fetch,
    });
    await client.persistentVolumeClaims.create({ metadata: { name: "tree-2" }, spec: {} });
    const fetched = await client.persistentVolumeClaims.get("tree-2");
    expect(fetched.metadata.name).toBe("tree-2");
  });
});

describe("createK8sClient / events", () => {
  it("sends the encoded involvedObject.name field selector and filters", async () => {
    const api = createFakeK8sApi({ namespace: "legion", now: () => NOW });
    const scheduled: FakeK8sEvent = {
      metadata: { name: "e1" },
      reason: "Scheduled",
      involvedObject: { name: "legion-x-planner-g1" },
    };
    const other: FakeK8sEvent = {
      metadata: { name: "e2" },
      reason: "Pulled",
      involvedObject: { name: "other-pod" },
    };
    api.events.push(scheduled, other);

    const client = createK8sClient({
      server: "https://fake.test",
      namespace: "legion",
      fetch: api.fetch,
    });
    const events = await client.events.list("legion-x-planner-g1");
    expect(events.map((event) => event.reason)).toEqual(["Scheduled"]);

    const request = api.requests.find((entry) => entry.path.startsWith("/events"));
    expect(request?.path).toBe("/events?fieldSelector=involvedObject.name%3Dlegion-x-planner-g1");
  });
});

describe("createK8sClient / fetch init", () => {
  it("passes the bearer token and tls options straight to fetch", async () => {
    let capturedInit: (RequestInit & { tls?: unknown }) | undefined;
    const stubFetch = (async (_input: string | URL | Request, init?: RequestInit) => {
      capturedInit = init as RequestInit & { tls?: unknown };
      return new Response(JSON.stringify({ metadata: { name: "x" }, spec: {} }), { status: 200 });
    }) as typeof fetch;
    const tls = { ca: "CA", cert: "CERT", key: "KEY", rejectUnauthorized: false };
    const client = createK8sClient({
      server: "https://fake.test",
      namespace: "legion",
      token: "abc123",
      tls,
      fetch: stubFetch,
    });

    await client.pods.get("x");

    const headers = capturedInit?.headers as Record<string, string> | undefined;
    expect(headers?.Authorization).toBe("Bearer abc123");
    expect(capturedInit?.tls).toEqual(tls);
  });
});

describe("resolveK8sCredentials", () => {
  it("reads certificate-authority-data/client-certificate-data/client-key-data as tls", async () => {
    const dir = await tempDir("legion-k8s-kubeconfig-");
    try {
      const kubeconfigPath = path.join(dir, "config");
      const b64 = (value: string) => Buffer.from(value).toString("base64");
      await writeFile(
        kubeconfigPath,
        [
          "current-context: test",
          "contexts:",
          "  - name: test",
          "    context: { cluster: test, user: test }",
          "clusters:",
          "  - name: test",
          "    cluster:",
          "      server: https://127.0.0.1:6443",
          `      certificate-authority-data: ${b64("CA")}`,
          "users:",
          "  - name: test",
          "    user:",
          `      client-certificate-data: ${b64("CERT")}`,
          `      client-key-data: ${b64("KEY")}`,
          "",
        ].join("\n")
      );

      const credentials = await resolveK8sCredentials({ kubeconfig: kubeconfigPath });
      expect(credentials).toEqual({
        server: "https://127.0.0.1:6443",
        tls: { ca: "CA", cert: "CERT", key: "KEY" },
      });
    } finally {
      await rm(dir, { recursive: true, force: true });
    }
  });

  it("maps insecure-skip-tls-verify to rejectUnauthorized: false alongside a bearer token", async () => {
    const dir = await tempDir("legion-k8s-kubeconfig-insecure-");
    try {
      const kubeconfigPath = path.join(dir, "config");
      await writeFile(
        kubeconfigPath,
        [
          "current-context: test",
          "contexts:",
          "  - name: test",
          "    context: { cluster: test, user: test }",
          "clusters:",
          "  - name: test",
          "    cluster:",
          "      server: https://127.0.0.1:6443",
          "      insecure-skip-tls-verify: true",
          "users:",
          "  - name: test",
          "    user:",
          "      token: sometoken",
          "",
        ].join("\n")
      );

      const credentials = await resolveK8sCredentials({ kubeconfig: kubeconfigPath });
      expect(credentials).toEqual({
        server: "https://127.0.0.1:6443",
        token: "sometoken",
        tls: { rejectUnauthorized: false },
      });
    } finally {
      await rm(dir, { recursive: true, force: true });
    }
  });

  it("resolves in-cluster credentials from the service account directory", async () => {
    const dir = await tempDir("legion-k8s-sa-");
    try {
      await writeFile(path.join(dir, "token"), "sa-token\n");
      await writeFile(path.join(dir, "ca.crt"), "SA-CA\n");
      await writeFile(path.join(dir, "namespace"), "legion\n");

      const credentials = await resolveK8sCredentials({
        serviceAccountDir: dir,
        env: { KUBERNETES_SERVICE_HOST: "10.0.0.1", KUBERNETES_SERVICE_PORT: "443" },
      });
      expect(credentials).toEqual({
        server: "https://10.0.0.1:443",
        namespace: "legion",
        token: "sa-token",
        tls: { ca: "SA-CA" },
      });
    } finally {
      await rm(dir, { recursive: true, force: true });
    }
  });

  it("defaults to IN_CLUSTER_SERVICE_ACCOUNT_DIR when no serviceAccountDir is given", async () => {
    const files: Record<string, string> = {
      [`${IN_CLUSTER_SERVICE_ACCOUNT_DIR}/token`]: "tok\n",
      [`${IN_CLUSTER_SERVICE_ACCOUNT_DIR}/ca.crt`]: "CA\n",
      [`${IN_CLUSTER_SERVICE_ACCOUNT_DIR}/namespace`]: "legion\n",
    };
    const readFile = async (filePath: string) => {
      const content = files[filePath];
      if (content === undefined) throw new Error(`ENOENT: ${filePath}`);
      return content;
    };

    const credentials = await resolveK8sCredentials({
      env: { KUBERNETES_SERVICE_HOST: "host", KUBERNETES_SERVICE_PORT: "443" },
      readFile,
    });
    expect(credentials.server).toBe("https://host:443");
    expect(credentials.token).toBe("tok");
  });

  it("throws naming both the kubeconfig setting and the in-cluster token path when neither is available", async () => {
    const dir = await tempDir("legion-k8s-missing-sa-");
    await rm(dir, { recursive: true, force: true });

    await expect(resolveK8sCredentials({ serviceAccountDir: dir, env: {} })).rejects.toThrow(
      `runtime.kubernetes.kubeconfig is not set and ${dir}/token does not exist: the daemon runs neither in a pod nor with a kubeconfig`
    );
  });
});
