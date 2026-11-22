#!/usr/bin/env node
'use strict';

const http = require('node:http');
const { spawn, execSync } = require('node:child_process');

const GRAFANA_IMAGE = process.env.GRAFANA_IMAGE || 'grafana-enterprise';
const EXTRA_ARGS = process.argv.slice(2);

function parseSemver(tag) {
  const m = /^(\d+)\.(\d+)\.(\d+)$/.exec(tag);
  return m ? [Number(m[1]), Number(m[2]), Number(m[3])] : null;
}

function cmpSemverDesc(a, b) {
  for (let i = 0; i < 3; i++) {
    if (a.parts[i] !== b.parts[i]) {
      return b.parts[i] - a.parts[i];
    }
  }
  return 0;
}

const TARGET_MINORS = ['13.3', '13.2', '13.0', '12.4', '12.3'];

async function resolveVersions() {
  if (process.env.GRAFANA_VERSION) {
    return [process.env.GRAFANA_VERSION];
  }
  const url = `https://hub.docker.com/v2/repositories/grafana/${GRAFANA_IMAGE}/tags?page_size=100&ordering=last_updated`;
  const res = await fetch(url);
  if (!res.ok) {
    throw new Error(`Failed to fetch Grafana tags from Docker Hub (${res.status})`);
  }
  const data = await res.json();
  const parsed = (data.results || [])
    .map((t) => ({ tag: t.name, parts: parseSemver(t.name) }))
    .filter((t) => t.parts !== null)
    .sort(cmpSemverDesc);

  const byMinor = new Map();
  for (const entry of parsed) {
    const minorKey = `${entry.parts[0]}.${entry.parts[1]}`;
    if (!byMinor.has(minorKey)) {
      byMinor.set(minorKey, entry.tag);
    }
  }

  const versions = ['nightly'];
  for (const minor of TARGET_MINORS) {
    if (byMinor.has(minor)) {
      versions.push(byMinor.get(minor));
    }
  }
  return versions;
}

function runPlaywright() {
  return new Promise((resolve, reject) => {
    const stub = http.createServer((_, res) => {
      res.writeHead(200, { 'Content-Type': 'application/javascript' });
      res.end();
    });
    stub.listen(35729, '127.0.0.1', () => {
      const child = spawn('npx', ['playwright', 'test', ...EXTRA_ARGS], { stdio: 'inherit' });
      child.on('exit', (code) => {
        stub.close();
        if (code === 0) {
          resolve();
        } else {
          reject(new Error(`Playwright exited with code ${code ?? 1}`));
        }
      });
    });
  });
}

async function waitForGrafana(timeoutMs = 90_000) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    try {
      const res = await fetch('http://localhost:3000/login');
      if (res.ok) {
        return;
      }
    } catch {
      // Grafana not ready yet
    }
    await new Promise((r) => setTimeout(r, 2000));
  }
  throw new Error(`Grafana did not become ready within ${timeoutMs / 1000}s`);
}

async function main() {
  const versions = await resolveVersions();
  console.log(`Running e2e against grafana/${GRAFANA_IMAGE}: ${versions.join(', ')}`);

  const results = [];
  for (const version of versions) {
    console.log(`\n=== Grafana ${version} ===`);
    try {
      execSync('docker compose down --remove-orphans', { stdio: 'inherit' });
      execSync('docker compose up --remove-orphans -d --build', {
        stdio: 'inherit',
        env: { ...process.env, DEVELOPMENT: 'false', GRAFANA_IMAGE, GRAFANA_VERSION: version },
      });
      await waitForGrafana();
      await runPlaywright();
      results.push({ version, ok: true });
    } catch (err) {
      console.error(`Grafana ${version} failed: ${err.message}`);
      results.push({ version, ok: false, error: err.message });
    }
  }

  console.log('\n=== E2E Summary ===');
  for (const r of results) {
    console.log(`${r.ok ? 'PASS' : 'FAIL'}  ${r.version}${r.error ? ` (${r.error})` : ''}`);
  }
  if (results.some((r) => !r.ok)) {
    process.exit(1);
  }
}

main().catch((err) => {
  console.error(err.message);
  process.exit(1);
});
