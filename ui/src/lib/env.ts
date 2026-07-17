export interface EnvTemplateVariable {
  key: string;
  default: string;
  is_secret: boolean;
  operator_required: boolean;
  controller_managed: boolean;
}

export function isMissingEnvValue(value: string | undefined): boolean {
  return !value || value === '' || value === 'change_me';
}

export function missingRequiredEnvVariables(
  variables: EnvTemplateVariable[],
  overrides: Record<string, string>,
): EnvTemplateVariable[] {
  return variables.filter((variable) =>
    variable.operator_required &&
    !variable.controller_managed &&
    isMissingEnvValue(overrides[variable.key] ?? variable.default),
  );
}
