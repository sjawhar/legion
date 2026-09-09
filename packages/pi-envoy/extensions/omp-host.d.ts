declare module "@oh-my-pi/pi-coding-agent" {
  export function copyToClipboard(text: string): Promise<void>;

  export interface ExtensionAgent {
    readonly id: string;
  }

  export interface ExtensionAgentsApi {
    list(): readonly ExtensionAgent[];
    get(agentId: string): ExtensionAgent | undefined;
    ensureLive(
      agentId: string,
      options: { readonly parentSessionFile: string }
    ): Promise<ExtensionAgent>;
    prompt(agentId: string, content: string): Promise<void>;
  }

}
