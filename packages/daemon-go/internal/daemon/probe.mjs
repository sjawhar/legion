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
  const agents = await resolve("LEGION_PROMPT_AGENTS", async () => {
    const { discoverAgents } = await import("@oh-my-pi/pi-coding-agent/task/discovery");
    return (await discoverAgents(cwd, undefined, roots)).agents;
  });
  // Each of those agents' models, resolved once the session's model registry exists: Oh My Pi hands
  // it to extensions only in an event, and `omp models` emits session_shutdown.
  if (agents && !process.env.LEGION_SKIP_AGENT_MODELS) {
    pi.on("session_shutdown", (_event, ctx) => agentModels(agents, ctx));
  }
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

// resolve answers, under variable, which of the names it lists Oh My Pi's discovery finds, and
// returns the entries found among them.
async function resolve(variable, discover) {
  const wanted = (process.env[variable] ?? "").split(",").filter(Boolean);
  if (wanted.length === 0) return undefined;
  try {
    const discovered = await discover();
    const found = new Set(discovered.map((entry) => entry.name));
    const missing = wanted.filter((name) => !found.has(name));
    process.stderr.write(
      missing.length === 0 ? `${variable}=resolved\n` : `${variable}_MISSING=${missing.join(",")}\n`
    );
    return discovered.filter((entry) => wanted.includes(entry.name));
  } catch (error) {
    process.stderr.write(`${variable}_UNRESOLVABLE=${firstLine(error)}\n`);
    return undefined;
  }
}

// agentModels answers whether each agent runs on its own model, selected and resolved as the task
// tool does a subagent's at the pin (resolveEffectiveAgentModelSelection in config/model-resolver.ts,
// resolveModelOverrideWithAuthFallback from task/executor.ts): the operator's
// `task.agentModelOverrides` entry when it expands to a model, else the agent's frontmatter model,
// expanded through the configured roles, then resolved with the operator's default role as the
// parent's model, then the model's key. Where the task tool would quietly run the agent on the
// parent's model instead (a role with no configured model, or a model whose key does not work) the
// agent is unresolved. An agent that names no model runs on the session's own by design, and is
// not judged.
async function agentModels(agents, ctx) {
  try {
    const resolver = await import("@oh-my-pi/pi-coding-agent/config/model-resolver");
    const { isAuthenticated, kNoAuth } = await import("@oh-my-pi/pi-coding-agent/config/model-registry");
    const { settings } = await import("@oh-my-pi/pi-coding-agent/config/settings");
    const overrides = settings.get("task.agentModelOverrides") ?? {};
    const parent = settings.getModelRole("default");

    // problem is why the task tool would not run an agent on the model it declares, or undefined.
    const problem = async (declared) => {
      const patterns = resolver.resolveConfiguredModelPatterns(declared, settings);
      const result =
        patterns.length === 0
          ? {}
          : await resolver.resolveModelOverrideWithAuthFallback(patterns, parent, ctx.modelRegistry, settings);
      if (!result.model) {
        // A role alias no configured role expands stays an alias: the role is not configured.
        const left = patterns.length > 0 ? patterns : declared;
        const roles = left.filter((pattern) => pattern.startsWith("@"));
        if (roles.length === left.length) {
          return `role ${roles.map((role) => role.slice(1).split(":")[0]).join(",")} is not configured`;
        }
        return `no available model matches ${left.join(",")}`;
      }
      const model = `${result.model.provider}/${result.model.id}`;
      if (result.authFallbackUsed) {
        return `${patterns.join(",")} has no working credentials, so Oh My Pi runs it on the parent's model ${model}`;
      }
      const key = await ctx.modelRegistry.getApiKey(result.model);
      if (key !== kNoAuth && !isAuthenticated(key)) return `its model ${model} has no working credentials`;
      return undefined;
    };

    const unresolved = [];
    for (const agent of agents) {
      const override = overrides[agent.name];
      const declared = resolver.normalizeModelPatternList(
        resolver.resolveConfiguredModelPatterns(override, settings).length > 0 ? override : agent.model
      );
      if (declared.length === 0) continue;
      const why = await problem(declared);
      // <agent> <model> <why>: the gate splits the answer on its first two spaces, and a model
      // pattern holds none.
      if (why) unresolved.push(`${agent.name} ${declared.join(",")} ${why}`);
    }
    process.stderr.write(
      unresolved.length === 0
        ? "LEGION_AGENT_MODELS=resolved\n"
        : unresolved.map((line) => `LEGION_AGENT_MODEL_UNRESOLVED=${line}\n`).join("")
    );
  } catch (error) {
    process.stderr.write(`LEGION_AGENT_MODELS_UNRESOLVABLE=${firstLine(error)}\n`);
  }
}

function firstLine(error) {
  return String(error?.message ?? error).split("\n")[0];
}
