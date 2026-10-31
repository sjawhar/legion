// An OpenCode serve, as far as this plugin is concerned: it loads the plugin, runs the sessions
// named on its command line, says `ready` and then waits. A test signals this process to prove
// what the plugin does on the way out, which no in-process test can do — a handler that re-raises
// the signal would take the test runner with it.
import plugin from "../server";

const sessions = process.argv.slice(2);
const hooks = await plugin({ serverUrl: new URL("http://127.0.0.1:13381/") } as never);
for (const sessionID of sessions) {
  await hooks.event({
    event: { type: "session.status", properties: { sessionID, status: { type: "busy" } } },
  });
}
process.stdout.write("ready\n");
// Nothing else holds this process open; the signal handlers the plugin installed end it.
setInterval(() => {}, 1_000);
