// Entry point: start the forge binary that lives beside this file and wait
// for it. The binary binds its own ephemeral port, registers with the grading
// environment, serves, polls /done, and exits 0; this process exits with the
// same code the binary does.

import { spawn } from "node:child_process";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const here = dirname(fileURLToPath(import.meta.url));
const binary = join(here, "forge-bin");

const child = spawn(binary, [], {
  stdio: "inherit",
  env: process.env,
});

child.on("error", (err: Error) => {
  process.stderr.write(`forge-bin failed to start: ${err.message}\n`);
  process.exit(1);
});

child.on("exit", (code: number | null, signal: string | null) => {
  if (signal !== null) {
    process.stderr.write(`forge-bin stopped on ${signal}\n`);
    process.exit(1);
  }
  process.exit(code ?? 1);
});
