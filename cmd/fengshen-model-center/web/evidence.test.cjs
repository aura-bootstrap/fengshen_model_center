'use strict';
const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { label, isTerminal, storageFrozen } = require('./app.js');
const source = fs.readFileSync(path.join(__dirname, 'app.js'), 'utf8');

test('component evidence remains orthogonal and only explicit verification is exposed', () => {
  for (const field of ['packaging', 'platform', 'files_state', 'integrity', 'startup_test', 'protocol_evidence', 'inference']) assert.ok(source.includes(`['${field}',`));
  assert.match(label('pending_build'), /未交付/);
  assert.match(label('declared_not_tested'), /未执行验证/);
  assert.match(label('pyinstaller-onedir'), /Python 运行时/);
  assert.match(label('matched'), /非官方认证/);
  assert.ok(source.includes("'/api/operations/verify-component', { component_id: component.component_id, version: component.version }"));
  assert.doesNotMatch(source, /\/api\/(?:install|execute|inference|download|build)/);
});
test('all operation terminal statuses are final; cancelling is not completed', () => {
  for (const status of ['completed', 'partial', 'cancelled', 'failed']) assert.equal(isTerminal({ status }), true);
  for (const status of ['running', 'cancelling', 'unknown']) assert.equal(isTerminal({ status }), false);
  assert.equal(isTerminal(null), false);
});
test('storage_error freezes only explicit storage faults; session and stale guards remain', () => {
  for (const code of ['registry_corrupt', 'registry_external_change', 'registry_commit_uncertain', 'external_change', 'commit_uncertain']) {
    assert.equal(storageFrozen(`${code}: 存储故障`), true, code);
  }
  for (const error of [undefined, null, '', 'registration_unavailable: whisper-asr: missing', 'registration_changed: whisper-asr: changed', 'registry_write_failed: temporary', 'registry_conflict: revision', 'unknown: diagnostic',
    'registration_unavailable: W:\\registry_corrupt: not a freeze', 'registration_changed: whisper-asr: commit_uncertain']) {
    assert.equal(storageFrozen(error), false, String(error));
  }
  assert.match(source, /snapshot\.storage_error/);
  assert.doesNotMatch(source, /state\.registry\.error/);
  assert.match(source, /error\.status === 403 \|\| error\.code === 'session_invalid'/);
  assert.match(source, /state\.sessionInvalid = true; poller\.stop\(\)/);
  assert.match(source, /inventory\.registry_revision !== state\.registry\.revision \? \{ \.\.\.inventory, stale: true \}/);
  assert.match(source, /已回收|410|showError/);
});
