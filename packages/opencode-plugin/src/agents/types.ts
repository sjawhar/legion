export interface AgentDefinition {
  name: string;
  description: string;
  config: {
    model: string;
    temperature: number;
    prompt: string;
  };
}
