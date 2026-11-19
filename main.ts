// main.ts — Node entrypoint for the trial harness (it runs `node /bot/main.ts`).
// Spawns the Go binary `./forge` (same directory as this file) as a child
// process with stdio inherited, waits for it to exit, and exits with the
// child's exit code. The Go binary owns the whole harness lifecycle itself:
// ephemeral port, env registration, waiting for done (see main.go).

import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import path from 'node:path';

const binary = path.join(path.dirname(fileURLToPath(import.meta.url)), 'forge');

const child = spawn(binary, [], { stdio: 'inherit' });
child.on('error', (err) => {
  process.stderr.write(`[entry] failed to start ${binary}: ${err.message}\n`);
  process.exit(1);
});
child.on('exit', (code, signal) => {
  if (signal !== null) {
    process.stderr.write(`[entry] forge terminated by signal ${signal}\n`);
    process.exit(1);
  }
  process.exit(code ?? 1);
});
