export interface SchemaNode<E> {
  optional(): E;
  describe(text: string): E;
}

export interface SchemaApi<E extends SchemaNode<E>> {
  string(opts?: { min?: number; max?: number }): E;
  number(opts?: { int?: boolean; min?: number; max?: number }): E;
  boolean(): E;
  enum(values: readonly [string, ...string[]]): E;
  array(item: E, opts?: { min?: number; max?: number }): E;
  object(shape: Record<string, E>): E;
}

export type ToolArgumentsShape = Readonly<Record<string, unknown>>;
