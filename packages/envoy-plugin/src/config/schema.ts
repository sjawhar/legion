import { tool } from "@opencode-ai/plugin";

const z = tool.schema;

export const DispatchConfigSchema = z
  .object({
    enabled: z.boolean().optional(),
    serverUrl: z.string().url().optional(),
    defaultRepo: z
      .string()
      .regex(/^[^/]+\/[^/]+$/)
      .optional(),
    appClientId: z.string().optional(),
  })
  .strict();

export const EnvoyConfigSchema = z
  .object({
    $schema: z.string().optional(),
    natsUrls: z.array(z.string()).optional(),
    dispatch: DispatchConfigSchema.optional(),
  })
  .passthrough();

export type DispatchConfig = ReturnType<typeof DispatchConfigSchema.parse>;
export type EnvoyConfig = ReturnType<typeof EnvoyConfigSchema.parse>;
