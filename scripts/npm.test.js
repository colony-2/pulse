"use strict";
const { test } = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const os = require("node:os");
const crypto = require("node:crypto");
const { execFileSync, spawn } = require("node:child_process");
const { once } = require("node:events");
const { assetName, download, install } = require("../npm/pulse/scripts/postinstall");

test("platform names match c2j's four release targets", () => {
  for (const [platform, arch, suffix] of [
    ["linux", "x64", "Linux_x86_64"], ["linux", "arm64", "Linux_arm64"],
    ["darwin", "x64", "Darwin_x86_64"], ["darwin", "arm64", "Darwin_arm64"],
  ]) assert.equal(assetName("1.2.3", platform, arch), `pulse_1.2.3_${suffix}.tar.gz`);
  assert.throws(() => assetName("1.2.3", "win32", "x64"), /Unsupported/);
  assert.throws(() => assetName("../../bad"), /Invalid/);
});

test("downloads cannot downgrade to HTTP or redirect indefinitely", async () => {
  await assert.rejects(download("http://example.com", "unused"), /HTTPS/);
  await assert.rejects(download("https://example.com", "unused", 6), /redirects/);
});

function fixture(t, script = "#!/bin/sh\nprintf '%s\\n' \"$@\"\nexit 7\n") {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "pulse-npm-test-"));
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));
  fs.writeFileSync(path.join(root, "package.json"), JSON.stringify({ version: "1.2.3" }));
  fs.mkdirSync(path.join(root, "bin"));
  fs.copyFileSync("npm/pulse/bin/cli.js", path.join(root, "bin/cli.js"));
  fs.writeFileSync(path.join(root, "pulse"), script, { mode: 0o755 });
  const archive = path.join(root, assetName("1.2.3"));
  execFileSync("tar", ["-czf", archive, "-C", root, "pulse"]);
  let sum = crypto.createHash("sha256").update(fs.readFileSync(archive)).digest("hex");
  return {
    root,
    corrupt: () => { sum = "0".repeat(64); },
    fetch: async (url, output) => {
      assert.ok(url.startsWith("https://github.com/colony-2/pulse/releases/download/v1.2.3/"));
      if (url.endsWith("checksums.txt")) fs.writeFileSync(output, `${sum}  ${path.basename(archive)}\n`);
      else fs.copyFileSync(archive, output);
    },
  };
}

test("verified install forwards arguments and exit code; corruption preserves existing binary", async (t) => {
  const f = fixture(t);
  await install(f);
  const child = spawn(process.execPath, [path.join(f.root, "bin/cli.js"), "one argument", "--once"]);
  let output = "";
  child.stdout.on("data", (chunk) => { output += chunk; });
  assert.deepEqual(await once(child, "exit"), [7, null]);
  assert.equal(output, "one argument\n--once\n");
  const binary = path.join(f.root, "vendor/pulse");
  const original = fs.readFileSync(binary);
  f.corrupt();
  await assert.rejects(install(f), /Checksum mismatch/);
  assert.deepEqual(fs.readFileSync(binary), original);
});

test("launcher forwards SIGTERM to the controller", { timeout: 10000 }, async (t) => {
  const f = fixture(t, `#!${process.execPath}\nprocess.on('SIGTERM', () => process.exit(0));\nconsole.log('ready');\nsetInterval(() => {}, 1000);\n`);
  await install(f);
  const child = spawn(process.execPath, [path.join(f.root, "bin/cli.js")]);
  t.after(() => child.kill("SIGKILL"));
  await once(child.stdout, "data");
  const exited = once(child, "exit");
  child.kill("SIGTERM");
  assert.deepEqual(await exited, [0, null]);
});
