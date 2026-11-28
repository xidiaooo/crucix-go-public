// Builds the Go server binary and assembles the deploy output in dist/.
import { execFileSync } from 'node:child_process';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { ensureGo } from './go.mjs';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const dist = path.join(root, 'dist');

const go = ensureGo();

fs.rmSync(dist, { recursive: true, force: true });
fs.mkdirSync(dist, { recursive: true });

execFileSync(
  go,
  ['build', '-trimpath', '-ldflags', '-s -w', '-o', path.join(dist, 'crucix'), '.'],
  {
    cwd: path.join(root, 'src'),
    stdio: 'inherit',
    env: { ...process.env, CGO_ENABLED: '0', GOTOOLCHAIN: 'local' },
  },
);

fs.copyFileSync(path.join(root, 'server.mjs'), path.join(dist, 'server.mjs'));
console.log('[build] wrote dist/crucix and dist/server.mjs');
