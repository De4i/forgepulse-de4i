import { spawn } from 'node:child_process';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

// Trial entry point. The forge itself is the statically linked Go binary
// sitting beside this file: it binds an ephemeral port, registers it with the
// trial environment and long-polls for completion. This launcher only starts
// it, forwards stdio and propagates its exit code, so the trial sees one
// process tree and the forge's own exit status.
const here = dirname(fileURLToPath(import.meta.url));
const binary = join(here, 'forgepulse');

const child = spawn(binary, [], { stdio: 'inherit', env: process.env });

child.on('error', error => {
  process.stderr.write(`forgepulse: could not start ${binary}: ${error.message}\n`);
  process.exit(1);
});

child.on('exit', (code, signal) => {
  if (signal) {
    process.kill(process.pid, signal);
    return;
  }
  process.exit(code ?? 0);
});

for (const signal of ['SIGINT', 'SIGTERM']) {
  process.on(signal, () => child.kill(signal));
}
