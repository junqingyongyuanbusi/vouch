#!/usr/bin/env node
// Thin launcher: resolve the platform binary from the matching optional
// dependency and exec it with the same argv/stdio. No network, no telemetry.
const { spawnSync } = require("node:child_process");
const path = require("node:path");

const PLATFORMS = {
  "darwin-arm64": "@vouch/cli-darwin-arm64",
  "darwin-x64": "@vouch/cli-darwin-x64",
  "linux-x64": "@vouch/cli-linux-x64",
  "linux-arm64": "@vouch/cli-linux-arm64",
};

const key = `${process.platform}-${process.arch}`;
const pkg = PLATFORMS[key];
if (!pkg) {
  console.error(`vouch: unsupported platform ${key}; see https://vouch.dev/docs/installation`);
  process.exit(2);
}

let bin;
try {
  bin = path.join(path.dirname(require.resolve(`${pkg}/package.json`)), "bin", "vouch");
} catch {
  console.error(`vouch: platform package ${pkg} is missing; reinstall (npm install vouch)`);
  process.exit(2);
}

const res = spawnSync(bin, process.argv.slice(2), { stdio: "inherit" });
if (res.error) {
  console.error(`vouch: ${res.error.message}`);
  process.exit(2);
}
process.exit(res.status === null ? 2 : res.status);
