import type { RequestCredential } from "@legion/envoy-client/bearer"
import ky from "ky"

export type SendEnvoyMessageOptions = {
  readonly baseUrl: string
  /** What the send presents (`EnabledEnvoySettings.credential`): the listener's bearer, this
   *  machine's broker credential, which signs a DPoP proof for this request, or none, which
   *  sends no `Authorization` header, for a loopback listener that asks for none. */
  readonly credential: RequestCredential | undefined
  readonly targetSession: string
  readonly message: string
}

export async function sendEnvoyMessage(options: SendEnvoyMessageOptions): Promise<void> {
  const url = new URL("/v1/messages/send", options.baseUrl)
  const headers = (await options.credential?.headers("POST", url.toString())) ?? {}
  await ky.post(url, {
    json: {
      target_session: options.targetSession,
      message: options.message,
    },
    headers,
    retry: 0,
    timeout: 5_000,
  })
}
