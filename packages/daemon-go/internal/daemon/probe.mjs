export default async function probeLegionPluginLoaded(pi) {
  const loaded = globalThis[Symbol.for("legion.pi-envoy.legion-loaded")];
  process.stderr.write(
    loaded
      ? `LEGION_PLUGIN_LOADED=yes\nLEGION_PLUGIN_LOADED_FROM=${loaded}\n`
      : "LEGION_PLUGIN_LOADED=no\n"
  );
  // The task agents and skills Legion's prompts name, each resolved as a worker's use resolves it:
  // Oh My Pi's own agent and skill discovery over this launch's extension roots. A pod names its
  // one plugin root with discovery off (LEGION_PROMPT_ROOT); a pane discovers its installed plugins.
  // Skills are filtered as a session filters them, by the launch's own `skills` settings and
  // `disabledExtensions` (session-tools.ts #applyDiscoveredSkills at the pin).
  const root = process.env.LEGION_PROMPT_ROOT;
  const roots = root
    ? { explicit: [root], mode: "explicit-only", configured: [], configuredLevel: "user" }
    : undefined;
  const cwd = process.cwd();
  await resolve("LEGION_PROMPT_AGENTS", async () => {
    const { discoverAgents } = await import("@oh-my-pi/pi-coding-agent/task/discovery");
    return (await discoverAgents(cwd, undefined, roots)).agents;
  });
  await resolve("LEGION_PROMPT_SKILLS", async () => {
    const { loadSkills } = await import("@oh-my-pi/pi-coding-agent/extensibility/skills");
    const { settings } = await import("@oh-my-pi/pi-coding-agent/config/settings");
    const options = {
      ...settings.getGroup("skills"),
      cwd,
      disabledExtensions: settings.get("disabledExtensions") ?? [],
      extensionRoots: roots,
    };
    return (await loadSkills(options)).skills;
  });
}

// resolve answers, under variable, which of the names it lists Oh My Pi's discovery finds.
async function resolve(variable, discover) {
  const wanted = (process.env[variable] ?? "").split(",").filter(Boolean);
  if (wanted.length === 0) return;
  try {
    const found = new Set((await discover()).map((entry) => entry.name));
    const missing = wanted.filter((name) => !found.has(name));
    process.stderr.write(
      missing.length === 0 ? `${variable}=resolved\n` : `${variable}_MISSING=${missing.join(",")}\n`
    );
  } catch (error) {
    const message = String(error?.message ?? error).split("\n")[0];
    process.stderr.write(`${variable}_UNRESOLVABLE=${message}\n`);
  }
}
