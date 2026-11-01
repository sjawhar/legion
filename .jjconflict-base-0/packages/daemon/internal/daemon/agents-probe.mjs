export default function probeOmpAgents(pi) {
  process.stderr.write(pi.agents ? "LEGION_OMP_AGENTS=available\n" : "LEGION_OMP_AGENTS=missing\n");
}
