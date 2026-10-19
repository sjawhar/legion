/**
 * Whether this process is the controller the daemon launched (`controller: daemon`): a controller
 * session carrying a boot token, which the daemon hands only the processes it launches and
 * `legion controller start` drops from what it inherits.
 */
export function daemonLaunchedController(env: NodeJS.ProcessEnv): boolean {
  return (
    env.LEGION_CONTROLLER === "1" &&
    env.LEGION_TREE === undefined &&
    (env.LEGION_BOOT_TOKEN_FILE !== undefined || env.LEGION_BOOT_TOKEN !== undefined)
  );
}
