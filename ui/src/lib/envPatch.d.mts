export interface EnvPatch {
  overrides: Record<string, string>;
  clearKeys: string[];
}

export function buildEnvPatch(
  editableKeys: Iterable<string>,
  savedOverrides: Record<string, string>,
  currentValues: Record<string, string>,
): EnvPatch;
