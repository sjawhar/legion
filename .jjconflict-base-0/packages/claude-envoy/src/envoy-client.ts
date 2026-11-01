import ky from "ky"

export type SendEnvoyMessageOptions = {
  readonly baseUrl: string
  /** The listener's bearer (`envoySettings`); none sends no `Authorization` header, for a loopback
   *  listener that asks for none. */
  readonly token: string | undefined
  readonly targetSession: string
  readonly message: string
}

export async function sendEnvoyMessage(options: SendEnvoyMessageOptions): Promise<void> {
  await ky.post(new URL("/v1/messages/send", options.baseUrl), {
    json: {
      target_session: options.targetSession,
      message: options.message,
    },
    headers: options.token === undefined ? {} : { Authorization: `Bearer ${options.token}` },
    retry: 0,
    timeout: 5_000,
  })
}
