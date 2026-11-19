import { spawn } from "node:child_process";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const entryDirectory = dirname(fileURLToPath(import.meta.url));
const executable = resolve(entryDirectory, "forge-bin");

const child = spawn(executable, process.argv.slice(2), {
  cwd: entryDirectory,
  env: process.env,
  stdio: "inherit",
});

for (const signal of ["SIGINT", "SIGTERM", "SIGHUP"] as const) {
  process.on(signal, () => child.kill(signal));
}

child.once("error", (error: Error) => {
  process.stderr.write(`forge-bin failed to start: ${error.message}\n`);
  process.exit(1);
});

child.once("exit", (code: number | null, signal: string | null) => {
  if (signal) {
    process.stderr.write(`forge-bin stopped on ${signal}\n`);
  }
  process.exit(code ?? 1);
});
