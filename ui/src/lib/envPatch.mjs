/**
 * Build the explicit environment patch sent to the controller.
 *
 * Only keys supplied in editableKeys are considered. Omitted keys are
 * preserved, an empty value explicitly clears a saved key, and the masked
 * secret sentinel is never sent back as data.
 *
 * @param {Iterable<string>} editableKeys
 * @param {Record<string, string>} savedOverrides
 * @param {Record<string, string>} currentValues
 * @returns {{overrides: Record<string, string>, clearKeys: string[]}}
 */
export function buildEnvPatch(editableKeys, savedOverrides, currentValues) {
  const overrides = {};
  const clearKeys = [];

  for (const key of editableKeys) {
    const saved = savedOverrides[key];
    const current = currentValues[key];
    if (saved !== undefined && current === '') {
      clearKeys.push(key);
    } else if (
      current !== undefined
      && current !== ''
      && current !== saved
      && current !== '********'
    ) {
      overrides[key] = current;
    }
  }

  return { overrides, clearKeys: clearKeys.sort() };
}
