'use strict';

const crypto = require('node:crypto');
const fs = require('node:fs');
const https = require('node:https');
const os = require('node:os');
const path = require('node:path');
const { spawn } = require('node:child_process');

const maxBinaryBytes = 100 * 1024 * 1024;

function verifiedBinary(data, expected) {
  if (!/^[a-f0-9]{64}$/i.test(expected || '')) {
    throw new Error('binary-sha256 must be 64 hexadecimal characters');
  }
  const digest = crypto.createHash('sha256').update(data).digest('hex');
  if (!crypto.timingSafeEqual(Buffer.from(digest, 'hex'), Buffer.from(expected, 'hex'))) {
    throw new Error('downloaded cluster executable failed SHA-256 verification');
  }
  return data;
}

function readLocalBinary(file, base) {
  const resolved = path.resolve(base, file);
  let stat;
  try { stat = fs.statSync(resolved); } catch { throw new Error('binary-path does not exist'); }
  if (!stat.isFile()) throw new Error('binary-path must be a regular file');
  if (stat.size > maxBinaryBytes) throw new Error('binary-path exceeds 100 MiB');
  return fs.readFileSync(resolved);
}

const releaseArch = { x64: 'amd64', arm64: 'arm64' };

function latestTag(base, repo) {
  const target = new URL(`${base}/${repo}/releases/latest`);
  return new Promise((resolve, reject) => {
    const request = https.get(target, { timeout: 30000 }, response => {
      response.resume();
      const location = response.headers.location;
      if (![301, 302, 303, 307, 308].includes(response.statusCode) || !location) {
        return reject(new Error(`no published release found for ${repo}`));
      }
      let tag = '';
      try {
        const match = /\/releases\/tag\/([^/]+)$/.exec(new URL(location, target).pathname);
        if (match) tag = decodeURIComponent(match[1]);
      } catch { /* rejected below */ }
      if (!/^[A-Za-z0-9._+-]+$/.test(tag)) {
        return reject(new Error(`unexpected latest release location for ${repo}`));
      }
      resolve(tag);
    });
    request.on('timeout', () => request.destroy(new Error('latest release lookup timed out')));
    request.on('error', reject);
  });
}

function checksumFor(sums, name) {
  for (const line of sums.split(/\r?\n/)) {
    const match = /^([a-f0-9]{64}) [ *](.+)$/i.exec(line);
    if (match && match[2] === name) return match[1];
  }
  throw new Error(`SHA256SUMS has no entry for ${name}`);
}

async function loadLatestRelease(env, deps) {
  const repo = env.GITHUB_ACTION_REPOSITORY || env.GITHUB_REPOSITORY || '';
  if (!/^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/.test(repo)) {
    throw new Error('one of binary-url or binary-path is required outside a GitHub Actions run');
  }
  const arch = releaseArch[deps.arch];
  if (deps.platform !== 'linux' || !arch) throw new Error(`no release binary for ${deps.platform}/${deps.arch}`);
  const base = (env.GITHUB_SERVER_URL || 'https://github.com').replace(/\/+$/, '');
  const tag = await deps.latestTag(base, repo);
  const name = `hvst-runner-gw-cluster-linux-${arch}`;
  const releaseBase = `${base}/${repo}/releases/download/${encodeURIComponent(tag)}`;
  console.log(`Downloading ${name} from ${repo} release ${tag}`);
  const sha = env['INPUT_BINARY-SHA256'] || checksumFor((await deps.download(`${releaseBase}/SHA256SUMS`)).toString('utf8'), name);
  return verifiedBinary(await deps.download(`${releaseBase}/${name}`), sha);
}

async function loadBinary(env = process.env, deps = {}) {
  const url = env['INPUT_BINARY-URL'] || '';
  const file = env['INPUT_BINARY-PATH'] || '';
  const sha = env['INPUT_BINARY-SHA256'] || '';
  if (url && file) throw new Error('binary-url and binary-path are mutually exclusive');
  if (!url && !file) {
    return loadLatestRelease(env, { download, latestTag, arch: process.arch, platform: process.platform, ...deps });
  }
  if (url) {
    if (!/^[a-f0-9]{64}$/i.test(sha)) throw new Error('binary-sha256 must be 64 hexadecimal characters');
    return verifiedBinary(await download(url), sha);
  }
  const data = readLocalBinary(file, env.GITHUB_WORKSPACE || process.cwd());
  return sha ? verifiedBinary(data, sha) : data;
}

function download(url, redirects = 0) {
  let target;
  try { target = new URL(url); } catch { return Promise.reject(new Error('binary-url must be a valid HTTPS URL')); }
  if (target.protocol !== 'https:' || target.username || target.password || redirects > 5) {
    return Promise.reject(new Error('binary-url must be HTTPS with at most five redirects'));
  }
  return new Promise((resolve, reject) => {
    const request = https.get(target, { timeout: 30000 }, response => {
      if ([301, 302, 303, 307, 308].includes(response.statusCode)) {
        const location = response.headers.location;
        response.resume();
        if (!location) return reject(new Error('binary download redirect has no location'));
        try {
          return download(new URL(location, target).toString(), redirects + 1).then(resolve, reject);
        } catch {
          return reject(new Error('binary download has an invalid redirect location'));
        }
      }
      if (response.statusCode !== 200) {
        response.resume();
        return reject(new Error(`binary download returned HTTP ${response.statusCode}`));
      }
      const chunks = [];
      let size = 0;
      response.on('data', chunk => {
        size += chunk.length;
        if (size > maxBinaryBytes) {
          response.destroy(new Error('binary download exceeds 100 MiB'));
          return;
        }
        chunks.push(chunk);
      });
      response.on('end', () => resolve(Buffer.concat(chunks)));
      response.on('error', reject);
    });
    request.on('timeout', () => request.destroy(new Error('binary download timed out')));
    request.on('error', reject);
  });
}

function saveState(key, value) {
  if (!process.env.GITHUB_STATE || /[\r\n]/.test(value)) throw new Error('GITHUB_STATE is unavailable or invalid');
  fs.appendFileSync(process.env.GITHUB_STATE, `${key}=${value}${os.EOL}`);
}

function run(binary, command) {
  return new Promise((resolve, reject) => {
    const child = spawn(binary, [command], { stdio: 'inherit', env: process.env });
    child.on('error', reject);
    child.on('exit', (code, signal) => code === 0 ? resolve() : reject(new Error(`cluster ${command} exited with ${code ?? signal}`)));
  });
}

async function main(command = 'create') {
  if (command !== 'create' && command !== 'create-k3s') throw new Error(`unknown cluster command ${command}`);
  const temp = process.env.RUNNER_TEMP;
  if (!temp) throw new Error('RUNNER_TEMP is required');
  const dir = fs.mkdtempSync(path.join(temp, 'hvst-cluster-bin-'));
  fs.chmodSync(dir, 0o700);
  const binary = path.join(dir, 'hvst-runner-gw-cluster');
  let registered = false;
  try {
    const data = await loadBinary();
    fs.writeFileSync(binary, data, { mode: 0o700 });
    saveState('binary_path', binary);
    registered = true;
    await run(binary, command);
  } catch (error) {
    if (!registered) fs.rmSync(dir, { recursive: true, force: true });
    throw error;
  }
}

async function post() {
  const binary = process.env.STATE_binary_path;
  if (!binary) return;
  const temp = process.env.RUNNER_TEMP;
  const dir = path.dirname(binary);
  if (!temp || path.dirname(dir) !== path.resolve(temp) || !path.basename(dir).startsWith('hvst-cluster-bin-') || path.basename(binary) !== 'hvst-runner-gw-cluster') {
    throw new Error('invalid saved cluster executable path');
  }
  try {
    await run(binary, 'cleanup');
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
}

module.exports = { main, post, verifiedBinary, download, run, loadBinary, checksumFor };
