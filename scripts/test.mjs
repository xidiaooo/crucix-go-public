// Runs the Go self tests (go test ./...). The tests read the tunnel
// credentials the server writes on first start and talk to a live server on
// 127.0.0.1:3117, so this script generates the credentials and starts the
// built binary before running go test, and stops it afterwards.
import { execFileSync, spawn } from 'node:child_process';
import fs from 'node:fs';
import net from 'node:net';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { ensureGo } from './go.mjs';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const bin = path.join(root, 'dist', 'crucix');
const port = 3117;

function probePort(port) {
  return new Promise((resolve) => {
    const socket = net.connect({ port, host: '127.0.0.1', timeout: 500 });
    socket.on('connect', () => {
      socket.destroy();
      resolve(true);
    });
    socket.on('error', () => resolve(false));
    socket.on('timeout', () => {
      socket.destroy();
      resolve(false);
    });
  });
}

async function main() {
  if (!fs.existsSync(bin)) {
    console.error('[test] dist/crucix is missing; run `npm run build` first.');
    process.exit(1);
  }

  const credsFile = path.join(root, 'src', 'runs', '.creds.json');
  let child = null;

  if (await probePort(port)) {
    console.log(`[test] a server already answers on 127.0.0.1:${port}; running the tests against it`);
  } else {
    if (!fs.existsSync(credsFile)) {
      fs.mkdirSync(path.dirname(credsFile), { recursive: true });
      console.log('[test] generating tunnel credentials for the self tests');
      const once = spawn(bin, [], {
        env: { ...process.env, RUNS_DIR: path.dirname(credsFile), PORT: String(port) },
        stdio: 'ignore',
      });
      const started = Date.now();
      while (!fs.existsSync(credsFile) && once.exitCode === null && Date.now() - started < 15000) {
        await new Promise((resolve) => setTimeout(resolve, 100));
      }
      once.kill('SIGKILL');
      if (!fs.existsSync(credsFile)) {
        console.error('[test] the server did not write credentials; cannot run the self tests.');
        process.exit(1);
      }
    }

    console.log(`[test] starting the server on 127.0.0.1:${port}`);
    child = spawn(bin, [], {
      env: { ...process.env, RUNS_DIR: path.dirname(credsFile), PORT: String(port) },
      stdio: 'ignore',
    });
    const started = Date.now();
    while (!(await probePort(port)) && child.exitCode === null && Date.now() - started < 15000) {
      await new Promise((resolve) => setTimeout(resolve, 100));
    }
    if (!(await probePort(port))) {
      console.error('[test] the server did not start; cannot run the self tests.');
      process.exit(1);
    }
  }

  let status = 0;
  try {
    const go = ensureGo();
    execFileSync(go, ['test', './...'], { cwd: path.join(root, 'src'), stdio: 'inherit' });
  } catch (err) {
    status = err.status ?? 1;
  } finally {
    if (child) {
      child.kill('SIGTERM');
      await new Promise((resolve) => setTimeout(resolve, 500));
      child.kill('SIGKILL');
    }
  }
  process.exit(status);
}

main();
