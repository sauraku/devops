import assert from 'node:assert/strict';
import test from 'node:test';

import { buildEnvPatch } from './envPatch.mjs';

test('buildEnvPatch sends only edits and explicit clears', () => {
  const editableKeys = new Set(['SMTP_HOST', 'SMTP_PASS', 'SMTP_USER']);
  const saved = {
    SMTP_HOST: 'smtp.old.example',
    SMTP_PASS: '********',
    SMTP_USER: 'mailer',
    COMPOSE_PROFILES: 'must-not-be-sent',
  };
  const current = {
    SMTP_HOST: 'smtp.new.example',
    SMTP_PASS: '********',
    SMTP_USER: '',
    COMPOSE_PROFILES: 'debug',
  };

  assert.deepEqual(buildEnvPatch(editableKeys, saved, current), {
    overrides: { SMTP_HOST: 'smtp.new.example' },
    clearKeys: ['SMTP_USER'],
  });
});

test('buildEnvPatch preserves omitted and unchanged values', () => {
  assert.deepEqual(
    buildEnvPatch(
      new Set(['SMTP_HOST', 'SMTP_PASS', 'SMTP_FROM']),
      { SMTP_HOST: 'smtp.example', SMTP_PASS: '********', SMTP_FROM: 'orders@example' },
      { SMTP_HOST: 'smtp.example', SMTP_PASS: '********' },
    ),
    { overrides: {}, clearKeys: [] },
  );
});

test('buildEnvPatch never sends masked values or controller-managed keys', () => {
  assert.deepEqual(
    buildEnvPatch(
      new Set(['SMTP_PASS']),
      { SMTP_PASS: '********', COMPOSE_PROFILES: 'legacy' },
      { SMTP_PASS: '********', COMPOSE_PROFILES: 'forged' },
    ),
    { overrides: {}, clearKeys: [] },
  );
});
