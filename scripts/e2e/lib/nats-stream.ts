// The two JetStream reads and the one write a stage proof makes of a stream it does not own: whether
// the stream carries a subject, the durable consumers of a project, and the deletion of the run's
// own durable consumers at teardown. It reads what the stream holds and never publishes.
//
//   bun scripts/e2e/lib/nats-stream.ts last <nats-url> <stream> <subject-filter> <seconds>
//     prints {"subject","time","seq"} of the newest message on the filter (wildcards allowed), read
//     through an ordered consumer that starts at the last message per subject; exits 1 when the
//     stream holds none within <seconds>
//   bun scripts/e2e/lib/nats-stream.ts consumers <nats-url> <stream> <name-prefix>
//     prints one {"name","filters","pending"} line per durable consumer whose name starts with the
//     prefix
//   bun scripts/e2e/lib/nats-stream.ts delete <nats-url> <stream> <name>
//     deletes one durable consumer and prints "deleted"; one that does not exist is already gone:
//     it prints "absent" and exits 0 too, so a caller says which it was
import { connect, DeliverPolicy } from "nats";

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

const nc = await connect({ servers: url, timeout: 10_000, name: "legion-e2e-nats-stream" });
try {
  if (command === "last") {
    const seconds = Number(extra ?? "10");
    if (!Number.isFinite(seconds) || seconds <= 0)
      refuse(`seconds must be a positive number, not ${extra}`);
    const js = nc.jetstream();
    const consumer = await js.consumers.get(stream, {
      filterSubjects: [argument],
      deliver_policy: DeliverPolicy.LastPerSubject,
    });
    let newest: { subject: string; time: string; seq: number } | undefined;
    const deadline = Date.now() + seconds * 1000;
    // Last-per-subject delivers the newest message of every matching subject; the newest of those
    // is the stream's newest message on the filter.
    while (Date.now() < deadline) {
      const message = await consumer.next({
        expires: Math.min(2000, Math.max(deadline - Date.now(), 100)),
      });
      if (!message) break;
      const time = new Date(message.info.timestampNanos / 1e6).toISOString();
      if (!newest || message.seq > newest.seq)
        newest = { subject: message.subject, time, seq: message.seq };
      if (message.info.pending === 0) break;
    }
    if (!newest) {
      process.stderr.write(`nats-stream: stream ${stream} holds no message on ${argument}\n`);
      process.exitCode = 1;
    } else {
      process.stdout.write(`${JSON.stringify(newest)}\n`);
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
