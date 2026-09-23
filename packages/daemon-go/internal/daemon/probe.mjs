export default function probeLegionPluginLoaded(pi) {
  const loaded = globalThis[Symbol.for("legion.pi-envoy.legion-loaded")];
  process.stderr.write(
    loaded
      ? `LEGION_PLUGIN_LOADED=yes\nLEGION_PLUGIN_LOADED_FROM=${loaded}\n`
      : "LEGION_PLUGIN_LOADED=no\n"
  );
}
