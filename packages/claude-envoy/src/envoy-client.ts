import type { RequestCredential } from "@legion/envoy-client/bearer"
import ky from "ky"

export type SendEnvoyMessageOptions = {
  readonly baseUrl: string
  /** What the send presents (`envoyCredential`): the listener's bearer, this machine's broker
   *  credential, which signs a DPoP proof for this request, or none, which sends no
   *  `Authorization` header, for a loopback listener that asks for none. */
  readonly token: string | RequestCredential | undefined
  readonly targetSession: string
  readonly message: string
}

export async function sendEnvoyMessage(options: SendEnvoyMessageOptions): Promise<void> {
  const url = new URL("/v1/messages/send", options.baseUrl)
  const headers =
    options.token === undefined || options.token === ""
      ? {}
      : typeof options.token === "string"
        ? { Authorization: `Bearer ${options.token}` }
        : await options.token.headers("POST", url.toString())
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
