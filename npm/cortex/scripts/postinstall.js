#!/usr/bin/env node
"use strict";

// Adapted from colony-2/c2j's checksum-verified release installer. Changes:
// bounded HTTPS transfers, atomic installation, and testable entry points.
const crypto = require("node:crypto");
const fs = require("node:fs");
const https = require("node:https");
const os = require("node:os");
const path = require("node:path");
const { execFileSync } = require("node:child_process");
const { pipeline } = require("node:stream/promises");

const platforms = {
  "linux:x64": ["Linux", "x86_64"],
  "linux:arm64": ["Linux", "arm64"],
  "darwin:x64": ["Darwin", "x86_64"],
  "darwin:arm64": ["Darwin", "arm64"],
};

function assetName(version, platform = process.platform, arch = process.arch) {
  const target = platforms[`${platform}:${arch}`];
  if (!target) throw new Error(`Unsupported Cortex platform: ${platform}/${arch}`);
  if (!/^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?$/.test(version)) {
    throw new Error(`Invalid Cortex release version: ${version}`);
  }
  return `cortex_${version}_${target[0]}_${target[1]}.tar.gz`;
}

function download(url, destination, redirects = 0) {
  return new Promise((resolve, reject) => {
    if (new URL(url).protocol !== "https:" || redirects > 5) {
      reject(new Error("Release downloads require HTTPS and at most five redirects"));
      return;
    }
    const request = https.get(url, (response) => {
      if (response.statusCode >= 300 && response.statusCode < 400 && response.headers.location) {
        response.resume();
        download(new URL(response.headers.location, url).href, destination, redirects + 1).then(resolve, reject);
        return;
      }
      if (response.statusCode !== 200) {
        response.resume();
        reject(new Error(`Release download failed: HTTP ${response.statusCode}`));
        return;
      }
      pipeline(response, fs.createWriteStream(destination, { mode: 0o600 })).then(resolve, reject);
    });
    request.setTimeout(30_000, () => request.destroy(new Error("Release download timed out")));
    const deadline = setTimeout(() => request.destroy(new Error("Release download exceeded five minutes")), 300_000);
    request.on("close", () => clearTimeout(deadline));
    request.on("error", reject);
  });
}

async function downloadWithRetry(url, destination) {
  for (let attempt = 1; ; attempt++) {
    try {
      await download(url, destination);
      return;
    } catch (error) {
      if (attempt === 5) throw error;
      await new Promise((resolve) => setTimeout(resolve, attempt * 1000));
    }
  }
}

function verifyChecksum(archive, checksums, name) {
  const entries = fs.readFileSync(checksums, "utf8").trim().split(/\r?\n/)
    .map((line) => line.trim().split(/\s+/)).filter((parts) => parts[1] === name);
  if (entries.length !== 1 || !/^[a-f0-9]{64}$/.test(entries[0][0])) {
    throw new Error(`Missing or ambiguous checksum for ${name}`);
  }
  const actual = crypto.createHash("sha256").update(fs.readFileSync(archive)).digest("hex");
  if (actual !== entries[0][0]) throw new Error(`Checksum mismatch for ${name}`);
}

async function install({ root = path.resolve(__dirname, ".."), fetch = downloadWithRetry } = {}) {
  const pkg = JSON.parse(fs.readFileSync(path.join(root, "package.json"), "utf8"));
  const name = assetName(pkg.version);
  const base = `https://github.com/colony-2/cortex/releases/download/v${pkg.version}`;
  const temp = fs.mkdtempSync(path.join(os.tmpdir(), "cortex-install-"));
  const vendor = path.join(root, "vendor");
  let staged;
  try {
    const archive = path.join(temp, name);
    const checksums = path.join(temp, "checksums.txt");
    await fetch(`${base}/${name}`, archive);
    await fetch(`${base}/checksums.txt`, checksums);
    verifyChecksum(archive, checksums, name);
    // Extract only the expected binary, never arbitrary archive paths.
    execFileSync("tar", ["-xzf", archive, "-C", temp, "cortex"], { stdio: "inherit" });
    const extracted = path.join(temp, "cortex");
    if (!fs.lstatSync(extracted).isFile()) throw new Error("Release binary is not a regular file");
    fs.mkdirSync(vendor, { recursive: true });
    staged = path.join(vendor, `.cortex-${process.pid}-${crypto.randomBytes(6).toString("hex")}`);
    fs.copyFileSync(extracted, staged);
    fs.chmodSync(staged, 0o755);
    fs.renameSync(staged, path.join(vendor, "cortex"));
  } finally {
    if (staged) fs.rmSync(staged, { force: true });
    fs.rmSync(temp, { recursive: true, force: true });
  }
}

if (require.main === module) {
  install().catch((error) => {
    console.error(error.message);
    process.exitCode = 1;
  });
}
module.exports = { assetName, download, install, verifyChecksum };
