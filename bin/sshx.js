#!/usr/bin/env node
import { spawnSync } from "node:child_process";
import {
  chmodSync,
  existsSync,
  mkdirSync,
  readFileSync,
  renameSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { homedir, tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const platformByNode = new Map([
  ["darwin", "darwin"],
  ["linux", "linux"],
  ["win32", "windows"],
]);

const archByNode = new Map([
  ["arm64", "arm64"],
  ["x64", "amd64"],
]);

const platform = platformByNode.get(process.platform);
const arch = archByNode.get(process.arch);

if (!platform || !arch) {
  console.error(`sshx: unsupported platform ${process.platform}/${process.arch}`);
  process.exit(1);
}

const root = dirname(dirname(fileURLToPath(import.meta.url)));
const packageJson = JSON.parse(readFileSync(join(root, "package.json"), "utf8"));
const version = packageJson.version;
const binaryName = `sshx-${platform}-${arch}${platform === "windows" ? ".exe" : ""}`;
const cacheRoot =
  process.env.SSHX_CACHE_DIR ||
  process.env.XDG_CACHE_HOME ||
  (process.platform === "win32"
    ? join(process.env.LOCALAPPDATA || tmpdir(), "sshx")
    : join(homedir(), ".cache", "sshx"));
const binaryPath = join(cacheRoot, version, binaryName);

if (!existsSync(binaryPath)) {
  await downloadBinary(binaryPath);
}

const result = spawnSync(binaryPath, process.argv.slice(2), {
  stdio: "inherit",
  env: process.env,
});

if (result.error) {
  console.error(`sshx: failed to launch native binary: ${result.error.message}`);
  process.exit(1);
}

process.exit(result.status ?? 0);

async function downloadBinary(destination) {
  const baseUrl =
    process.env.SSHX_RELEASE_BASE_URL ||
    "https://github.com/xiaot623/sshx/releases/download";
  const url = `${baseUrl}/v${version}/${binaryName}`;
  const tmpPath = `${destination}.${process.pid}.tmp`;

  mkdirSync(dirname(destination), { recursive: true });
  rmSync(tmpPath, { force: true });

  console.error(`sshx: downloading ${url}`);

  const response = await fetch(url);
  if (!response.ok) {
    console.error(`sshx: GitHub returned ${response.status} ${response.statusText}`);
    console.error("sshx: failed to download native binary from GitHub Release");
    process.exit(1);
  }

  writeFileSync(tmpPath, Buffer.from(await response.arrayBuffer()));

  if (platform !== "windows") {
    chmodSync(tmpPath, 0o755);
  }

  renameSync(tmpPath, destination);
}
