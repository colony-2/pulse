#!/usr/bin/env node
"use strict";

// Adapted from colony-2/c2j's npm launcher. Pulse forwards termination signals
// so a long-running controller shuts down when its npm launcher is stopped.
const fs = require("node:fs");
const path = require("node:path");
const { spawn } = require("node:child_process");
const binary = path.resolve(__dirname, "../vendor/pulse");

if (!fs.existsSync(binary)) {
  console.error("Pulse binary is missing. Run npm rebuild @colony2/pulse with install scripts enabled.");
  process.exit(1);
}

const child = spawn(binary, process.argv.slice(2), { stdio: "inherit" });
const handlers = new Map();
for (const signal of ["SIGINT", "SIGTERM", "SIGHUP"]) {
  const handler = () => child.kill(signal);
  handlers.set(signal, handler);
  process.on(signal, handler);
}
child.on("error", (error) => {
  console.error(error.message);
  process.exitCode = 1;
});
child.on("exit", (code, signal) => {
  for (const [name, handler] of handlers) process.removeListener(name, handler);
  if (signal) process.kill(process.pid, signal);
  else process.exitCode = code ?? 1;
});
