import { z } from "zod"

const SendArgumentsSchema = z.tuple([z.string().min(1), z.string().min(1)]).rest(z.string().min(1))

export type SendArguments = {
  readonly targetSession: string
  readonly message: string
}

export function parseSendArguments(arguments_: readonly string[]): SendArguments {
  const [targetSession, ...words] = SendArgumentsSchema.parse(arguments_)
  return { targetSession, message: words.join(" ") }
}
