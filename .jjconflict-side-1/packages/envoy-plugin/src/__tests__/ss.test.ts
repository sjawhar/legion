import { describe, expect, test } from "bun:test";
import { portFromSsOutput } from "../ss";

const header = "State  Recv-Q Send-Q Local Address:Port  Peer Address:Port Process";

function listen(local: string, pid: number): string {
  return `LISTEN 0      511    ${local}  0.0.0.0:*  users:(("bun",pid=${pid},fd=6))`;
}

describe("portFromSsOutput", () => {
  test("returns null when the process id has no listening socket", () => {
    const output = [header, listen("127.0.0.1:4096", 99_999)].join("\n");

    expect(portFromSsOutput(output, 12_345)).toBeNull();
  });

  test("skips matching process rows whose local address column is malformed", () => {
    const output = [header, listen("not-a-local-address", 123)].join("\n");

    expect(portFromSsOutput(output, 123)).toBeNull();
  });

  test("ignores zero and negative local ports", () => {
    const output = [header, listen("127.0.0.1:0", 123), listen("127.0.0.1:-1", 123)].join("\n");

    expect(portFromSsOutput(output, 123)).toBeNull();
  });

  test("matches the full pid token before returning a port", () => {
    const output = [header, listen("127.0.0.1:9999", 123), listen("127.0.0.1:4444", 12)].join("\n");

    expect(portFromSsOutput(output, 12)).toBe(4444);
  });
});
