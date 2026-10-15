// The two JetStream reads and the one write a stage proof makes of a stream it does not own: whether
// the stream carries a subject, the durable consumers of a project, and the deletion of the run's
// own durable consumers at teardown. It reads what the stream holds and never publishes.
//
//   bun scripts/e2e/lib/nats-stream.ts last <nats-url> <stream> <subject-filter> <seconds>
//     prints {"subject","time","seq"} of the newest message on the filter (wildcards allowed), read
//     through one ordered consumer delivery that starts at the stream's last matching message; exits
//     1 when the stream holds none within <seconds>
//   bun scripts/e2e/lib/nats-stream.ts consumers <nats-url> <stream> <name-prefix>
//     prints one {"name","filters","pending"} line per durable consumer whose name starts with the
//     prefix
//   bun scripts/e2e/lib/nats-stream.ts delete <nats-url> <stream> <name>
//     deletes one durable consumer and prints "deleted"; one that does not exist is already gone:
//     it prints "absent" and exits 0 too, so a caller says which it was
import { connect, DeliverPolicy, type JsMsg } from "nats";
import type { NatsSeed } from "../../../packages/envoy-client/src/envoy-settings";
import { messageFor } from "../../../packages/envoy-client/src/errors";
import { natsAuthOptions } from "../../../packages/envoy-client/src/nats-auth";
import { operatorNatsSeed } from "./operator-nats-seed";

function refuse(message: string): never {
  process.stderr.write(`nats-stream: ${message}\n`);
  process.exit(2);
}

const [command, url, stream, argument, extra] = process.argv.slice(2);
if (!command || !url || !stream || !argument) {
  refuse(
    "usage: last|consumers|delete <nats-url> <stream> <subject-filter|name-prefix|name> [seconds]"
  );
}

// Production NATS: the operator's nkey user, when the server requires one, as a command run from
// the operator's shell resolves it (operator-nats-seed.ts).
let seed: NatsSeed | undefined;
try {
  seed = operatorNatsSeed(url, "<nats-url>", process.env, process.cwd());
} catch (error) {
  refuse(messageFor(error));
}
const nc = await connect({
  servers: url,
  timeout: 10_000,
  name: "legion-e2e-nats-stream",
  ...natsAuthOptions(seed),
});
try {
  if (command === "last") {
    const seconds = Number(extra ?? "10");
    if (!Number.isFinite(seconds) || seconds <= 0)
      refuse(`seconds must be a positive number, not ${extra}`);
    const js = nc.jetstream();
    const consumer = await js.consumers.get(stream, {
      filterSubjects: [argument],
      deliver_policy: DeliverPolicy.Last,
    });
    const timeoutMilliseconds = Math.ceil(seconds * 1000);
    let stopTimer = () => {};
    let message: JsMsg | null | undefined;
    try {
      message = await Promise.race([
        consumer.next({ expires: Math.max(1000, timeoutMilliseconds) }),
        new Promise<undefined>((resolve) => {
          const timer = setTimeout(resolve, timeoutMilliseconds);
          stopTimer = () => clearTimeout(timer);
        }),
      ]);
    } finally {
      stopTimer();
    }
    if (!message) {
      process.stderr.write(`nats-stream: stream ${stream} holds no message on ${argument}\n`);
      process.exitCode = 1;
    } else {
      const time = new Date(message.info.timestampNanos / 1e6).toISOString();
      process.stdout.write(
        `${JSON.stringify({ subject: message.subject, time, seq: message.seq })}\n`
      );
    }
  } else if (command === "consumers") {
    const jsm = await nc.jetstreamManager();
    for await (const info of jsm.consumers.list(stream)) {
      if (!info.name.startsWith(argument)) continue;
      const filters =
        info.config.filter_subjects ??
        (info.config.filter_subject ? [info.config.filter_subject] : []);
      process.stdout.write(
        `${JSON.stringify({ name: info.name, filters, pending: info.num_pending })}\n`
      );
    }
  } else if (command === "delete") {
    const jsm = await nc.jetstreamManager();
    try {
      await jsm.consumers.delete(stream, argument);
      process.stdout.write("deleted\n");
    } catch (error) {
      if (!String(error).includes("consumer not found")) throw error;
      process.stdout.write("absent\n");
    }
  } else {
    refuse(`unknown command ${command}`);
  }
} finally {
  await nc.close();
}
