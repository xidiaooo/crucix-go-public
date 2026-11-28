// Locates or downloads the Go toolchain used to build the server binary.
import { execFileSync } from 'node:child_process';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

export const goVersion = '1.22.12';
const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');

export function ensureGo() {
  for (const dir of (process.env.PATH || '').split(path.delimiter)) {
    if (!dir) continue;
    const p = path.join(dir, 'go');
    try {
      fs.accessSync(p, fs.constants.X_OK);
      return p;
    } catch {
      // keep looking
    }
  }
  const cache = path.join(root, '.go-toolchain');
  const goRoot = path.join(cache, 'go');
  const goBin = path.join(goRoot, 'bin', 'go');
  if (fs.existsSync(goBin)) return goBin;
  const goos = process.platform === 'darwin' ? 'darwin' : 'linux';
  const arch = { arm64: 'arm64', x64: 'amd64' }[process.arch] || 'amd64';
  const tarball = path.join(cache, `go${goVersion}.${goos}-${arch}.tar.gz`);
  fs.mkdirSync(cache, { recursive: true });
  if (!fs.existsSync(tarball)) {
    console.log(`[build] downloading Go ${goVersion} (${goos}-${arch})`);
    execFileSync(
      'curl',
      ['-fsSL', '-o', tarball, `https://dl.google.com/go/go${goVersion}.${goos}-${arch}.tar.gz`],
      { stdio: 'inherit' },
    );
  }
  fs.rmSync(goRoot, { recursive: true, force: true });
  execFileSync('tar', ['-xzf', tarball, '-C', cache], { stdio: 'inherit' });
  return goBin;
}
