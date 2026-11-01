import { SLACK_CHANNEL_ID_PATTERN } from "@legion/contracts";

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

/**
 * The Slack channels the daemon names for the controller it launches: LEGION_SLACK_CHANNELS,
 * comma-separated, which the daemon always sets from the project's `slack_channels` (empty for
 * none) and the operator's pod may not. It is required, and an entry that is not a channel id is
 * refused, naming it, rather than subscribed to as a topic no mention reaches. Read only for that
 * controller: any other session, whatever it inherited, hears no Slack channel, since only the
 * daemon's controller is granted the Slack bot token.
 */
export function daemonControllerSlackChannels(env: NodeJS.ProcessEnv): readonly string[] {
  const { LEGION_SLACK_CHANNELS: named } = env;
  if (named === undefined) {
    throw new Error(
      "LEGION_SLACK_CHANNELS is required for the controller the daemon launches: the project's slack_channels, comma-separated, empty for none"
    );
  }
  if (named.trim() === "") return [];
  return named.split(",").map((entry) => {
    const channel = entry.trim();
    if (!SLACK_CHANNEL_ID_PATTERN.test(channel)) {
      throw new Error(
        `LEGION_SLACK_CHANNELS entry ${JSON.stringify(channel)} is not a Slack channel id (C or G, then capital letters and digits)`
      );
    }
    return channel;
  });
}
