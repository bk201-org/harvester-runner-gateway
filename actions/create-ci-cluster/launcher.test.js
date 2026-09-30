'use strict';

const assert = require('node:assert/strict');
const crypto = require('node:crypto');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const test = require('node:test');
const { checksumFor, loadBinary, main, post, verifiedBinary } = require('./launcher');

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

test('loadBinary downloads the latest release when no url or path is given', async () => {
  const data = Buffer.from('release executable');
  const sha = crypto.createHash('sha256').update(data).digest('hex');
  const requested = [];
  const deps = {
    arch: 'x64',
    platform: 'linux',
    latestTag: async (base, repo) => { requested.push(`${base} ${repo}`); return 'v1.2.3'; },
    download: async url => {
      requested.push(url);
      if (url.endsWith('/SHA256SUMS')) {
        return Buffer.from(`${'0'.repeat(64)}  hvst-runner-gw-linux-amd64\n${sha}  hvst-runner-gw-client-linux-amd64\n`);
      }
      return data;
    },
  };
  const env = { GITHUB_ACTION_REPOSITORY: 'owner/repo' };
  assert.deepEqual(await loadBinary(env, deps), data);
  assert.deepEqual(requested, [
    'https://github.com owner/repo',
    'https://github.com/owner/repo/releases/download/v1.2.3/SHA256SUMS',
    'https://github.com/owner/repo/releases/download/v1.2.3/hvst-runner-gw-client-linux-amd64',
  ]);
  await assert.rejects(loadBinary({ ...env, 'INPUT_BINARY-SHA256': '1'.repeat(64) }, deps), /verification/);
  await assert.rejects(loadBinary(env, { ...deps, arch: 'ia32' }), /no release binary/);
  await assert.rejects(
    loadBinary(env, { ...deps, download: async url => url.endsWith('/SHA256SUMS') ? Buffer.from('') : data }),
    /no entry/,
  );
});

test('checksumFor finds text and binary mode entries', () => {
  const sums = `${'a'.repeat(64)}  one\n${'b'.repeat(64)} *two\n`;
  assert.equal(checksumFor(sums, 'one'), 'a'.repeat(64));
  assert.equal(checksumFor(sums, 'two'), 'b'.repeat(64));
  assert.throws(() => checksumFor(sums, 'three'), /no entry/);
});

test('loadBinary validates input combinations', async () => {
  await assert.rejects(loadBinary({}), /required/);
  await assert.rejects(loadBinary({ 'INPUT_BINARY-URL': 'https://example.com/x', 'INPUT_BINARY-PATH': 'x' }), /mutually exclusive/);
  await assert.rejects(loadBinary({ 'INPUT_BINARY-URL': 'https://example.com/x' }), /64 hexadecimal/);
});

test('binary verification rejects a wrong checksum', () => {
  const data = Buffer.from('client executable');
  const correct = crypto.createHash('sha256').update(data).digest('hex');
  assert.equal(verifiedBinary(data, correct), data);
  assert.throws(() => verifiedBinary(data, '0'.repeat(64)), /verification/);
  assert.throws(() => verifiedBinary(data, 'invalid'), /64 hexadecimal/);
});

test('post passes action state to the cleanup executable', async () => {
  const temp = fs.mkdtempSync(path.join(os.tmpdir(), 'launcher-test-'));
  const dir = fs.mkdtempSync(path.join(temp, 'hvst-cluster-bin-'));
  const binary = path.join(dir, 'hvst-runner-gw-client');
  const marker = path.join(temp, 'marker');
  fs.writeFileSync(binary, '#!/bin/sh\nprintf "%s:%s:%s" "$1" "$2" "$STATE_cluster_state" > "$TEST_MARKER"\n', { mode: 0o700 });
  const previous = { ...process.env };
  try {
    process.env.RUNNER_TEMP = temp;
    process.env.STATE_binary_path = binary;
    process.env.STATE_cluster_state = '/tmp/example-state';
    process.env.TEST_MARKER = marker;
    await post();
    assert.equal(fs.readFileSync(marker, 'utf8'), 'action:cleanup:/tmp/example-state');
    assert.equal(fs.existsSync(dir), false);
  } finally {
    process.env = previous;
    fs.rmSync(temp, { recursive: true, force: true });
  }
});

test('main passes the requested command to the client executable', async () => {
  const temp = fs.mkdtempSync(path.join(os.tmpdir(), 'launcher-main-'));
  const marker = path.join(temp, 'marker');
  fs.writeFileSync(path.join(temp, 'bin'), '#!/bin/sh\nprintf "%s:%s" "$1" "$2" > "$TEST_MARKER"\n', { mode: 0o700 });
  fs.writeFileSync(path.join(temp, 'state'), '');
  const previous = { ...process.env };
  try {
    Object.assign(process.env, {
      RUNNER_TEMP: temp,
      GITHUB_STATE: path.join(temp, 'state'),
      GITHUB_WORKSPACE: temp,
      'INPUT_BINARY-PATH': 'bin',
      TEST_MARKER: marker,
    });
    await main('create-k3s');
    assert.equal(fs.readFileSync(marker, 'utf8'), 'action:create-k3s');
    await assert.rejects(main('bogus'), /unknown cluster command/);
  } finally {
    process.env = previous;
    fs.rmSync(temp, { recursive: true, force: true });
  }
});
