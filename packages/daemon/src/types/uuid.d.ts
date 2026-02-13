declare module "uuid" {
  export function v5(name: string, namespace: string): string;
  export function validate(uuid: string): boolean;
}
