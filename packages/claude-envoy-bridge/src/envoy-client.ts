import ky from "ky"

export type SendEnvoyMessageOptions = {
  readonly baseUrl: string
  readonly targetSession: string
  readonly message: string
}

export async function sendEnvoyMessage(options: SendEnvoyMessageOptions): Promise<void> {
  await ky.post(new URL("/v1/messages/send", options.baseUrl), {
    json: {
      target_session: options.targetSession,
      message: options.message,
    },
    retry: 0,
    timeout: 5_000,
  })
}
