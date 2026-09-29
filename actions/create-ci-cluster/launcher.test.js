'use strict';

const assert = require('node:assert/strict');
const crypto = require('node:crypto');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const test = require('node:test');
const { loadBinary, post, verifiedBinary } = require('./launcher');

test('loadBinary reads a local path, optionally verifying the checksum', async () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'launcher-path-'));
  try {
    const data = Buffer.from('local executable');
    fs.writeFileSync(path.join(dir, 'bin'), data);
    const sha = crypto.createHash('sha256').update(data).digest('hex');
    const env = { GITHUB_WORKSPACE: dir, 'INPUT_BINARY-PATH': 'bin' };
    assert.deepEqual(await loadBinary(env), data);
    assert.deepEqual(await loadBinary({ ...env, 'INPUT_BINARY-SHA256': sha }), data);
    await assert.rejects(loadBinary({ ...env, 'INPUT_BINARY-SHA256': '0'.repeat(64) }), /verification/);
    await assert.rejects(loadBinary({ ...env, 'INPUT_BINARY-PATH': 'missing' }), /does not exist/);
    await assert.rejects(loadBinary({ ...env, 'INPUT_BINARY-PATH': '.' }), /regular file/);
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
});

test('loadBinary validates input combinations', async () => {
  await assert.rejects(loadBinary({}), /required/);
  await assert.rejects(loadBinary({ 'INPUT_BINARY-URL': 'https://example.com/x', 'INPUT_BINARY-PATH': 'x' }), /mutually exclusive/);
  await assert.rejects(loadBinary({ 'INPUT_BINARY-URL': 'https://example.com/x' }), /64 hexadecimal/);
});

test('binary verification rejects a wrong checksum', () => {
  const data = Buffer.from('cluster executable');
  const correct = crypto.createHash('sha256').update(data).digest('hex');
  assert.equal(verifiedBinary(data, correct), data);
  assert.throws(() => verifiedBinary(data, '0'.repeat(64)), /verification/);
  assert.throws(() => verifiedBinary(data, 'invalid'), /64 hexadecimal/);
});

test('post passes action state to the cleanup executable', async () => {
  const temp = fs.mkdtempSync(path.join(os.tmpdir(), 'launcher-test-'));
  const dir = fs.mkdtempSync(path.join(temp, 'hvst-cluster-bin-'));
  const binary = path.join(dir, 'hvst-runner-gw-cluster');
  const marker = path.join(temp, 'marker');
  fs.writeFileSync(binary, '#!/bin/sh\nprintf "%s:%s" "$1" "$STATE_cluster_state" > "$TEST_MARKER"\n', { mode: 0o700 });
  const previous = { ...process.env };
  try {
    process.env.RUNNER_TEMP = temp;
    process.env.STATE_binary_path = binary;
    process.env.STATE_cluster_state = '/tmp/example-state';
    process.env.TEST_MARKER = marker;
    await post();
    assert.equal(fs.readFileSync(marker, 'utf8'), 'cleanup:/tmp/example-state');
    assert.equal(fs.existsSync(dir), false);
  } finally {
    process.env = previous;
    fs.rmSync(temp, { recursive: true, force: true });
  }
});
