"use strict";
// Test the packed artifact against local release archives before publication.
const fs = require("node:fs");
const path = require("node:path");
const os = require("node:os");
const { execFileSync } = require("node:child_process");

async function main() {
  const [version, releaseDir = "dist/release"] = process.argv.slice(2);
  if (!/^\d+\.\d+\.\d+$/.test(version)) throw new Error("Expected release version");
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "pulse-pack-smoke-"));
  try {
    const archive = path.resolve(releaseDir, `colony2-pulse-${version}.tgz`);
    execFileSync("npm", ["install", "--prefix", dir, "--ignore-scripts", "--no-audit", "--no-fund", archive], { stdio: "inherit" });
    const root = path.join(dir, "node_modules/@colony2/pulse");
    const { install } = require(path.join(root, "scripts/postinstall.js"));
    await install({ root, fetch: async (url, output) => {
      fs.copyFileSync(path.join(releaseDir, path.basename(new URL(url).pathname)), output);
    } });
    const output = execFileSync(process.execPath, [path.join(root, "bin/cli.js"), "-version"], { encoding: "utf8" }).trim();
    if (output !== `pulse ${version}`) throw new Error(`Unexpected npm-installed version: ${output}`);
    console.log(`Packed npm install passed: ${output}`);
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
}
main().catch((error) => { console.error(error); process.exitCode = 1; });
