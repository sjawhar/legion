export default async function probeLegionPluginLoaded(pi) {
  const loaded = globalThis[Symbol.for("legion.pi-envoy.legion-loaded")];
  process.stderr.write(
    loaded
      ? `LEGION_PLUGIN_LOADED=yes\nLEGION_PLUGIN_LOADED_FROM=${loaded}\n`
      : "LEGION_PLUGIN_LOADED=no\n"
  );
  // The task agents the plugin's skills dispatch, resolved as the task tool resolves a name: Oh My
  // Pi's own agent discovery over this launch's extension roots. A pod names its one plugin root
  // with discovery off (LEGION_PROMPT_AGENTS_ROOT); a pane discovers its installed plugins.
  const wanted = (process.env.LEGION_PROMPT_AGENTS ?? "").split(",").filter(Boolean);
  if (wanted.length === 0) return;
  try {
    const { discoverAgents } = await import("@oh-my-pi/pi-coding-agent/task/discovery");
    const root = process.env.LEGION_PROMPT_AGENTS_ROOT;
    const roots = root
      ? { explicit: [root], mode: "explicit-only", configured: [], configuredLevel: "user" }
      : undefined;
    const { agents } = await discoverAgents(process.cwd(), undefined, roots);
    const found = new Set(agents.map((agent) => agent.name));
    const missing = wanted.filter((name) => !found.has(name));
    process.stderr.write(
      missing.length === 0
        ? "LEGION_PROMPT_AGENTS=resolved\n"
        : `LEGION_PROMPT_AGENTS_MISSING=${missing.join(",")}\n`
    );
  } catch (error) {
    const message = String(error?.message ?? error).split("\n")[0];
    process.stderr.write(`LEGION_PROMPT_AGENTS_UNRESOLVABLE=${message}\n`);
  }
}
