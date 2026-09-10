import { defaultSchema } from "rehype-sanitize";

export const sanitizeSchema = {
  ...defaultSchema,
  attributes: {
    ...defaultSchema.attributes,
    "*": [...(defaultSchema.attributes?.["*"] ?? []), "dataDispatchSegments"],
  },
};
