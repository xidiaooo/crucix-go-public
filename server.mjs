// Runs the compiled Go server binary (dist/crucix) on the port the platform
// assigns, which arrives in the PORT environment variable and is bound by the
// Go server itself on every interface.
import { spawn } from 'node:child_process';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const bin = path.join(here, 'crucix');
const port = process.env.PORT || '3000';

const child = spawn(bin, [], {
  env: { ...process.env, PORT: port },
  stdio: ['ignore', 'inherit', 'inherit'],
});

child.on('error', (err) => {
  console.error('[crucix] failed to start server binary:', err);
  process.exit(1);
});

child.on('exit', (code, signal) => {
  if (signal) {
    process.kill(process.pid, signal);
  } else {
    process.exit(code ?? 1);
  }
});

for (const sig of ['SIGTERM', 'SIGINT']) {
  process.on(sig, () => {
    child.kill(sig);
  });
}
