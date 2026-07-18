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
import { dirname, isAbsolute, join, relative, resolve, sep } from "node:path";
import { fileURLToPath } from "node:url";

const npmRefreshArgument = "--npm-refresh-integrations";
const npmManagedMarker = ".npm-managed";
const integrationProfiles = ["vscode", "cursor"];

const platformByNode = new Map([
  ["darwin", "darwin"],
  ["linux", "linux"],
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
const refreshOnly =
  process.argv.length === 3 && process.argv[2] === npmRefreshArgument;
const integrationsToRefresh = findNpmIntegrationsToRefresh();

// Preserve lazy binary downloads on a first install that has no integrations yet.
if (refreshOnly && integrationsToRefresh.length === 0) {
  process.exit(0);
}

if (!existsSync(binaryPath)) {
  await downloadBinary(binaryPath);
}

const nativeEnvironment = {
  ...process.env,
  SSHX_NPM_LAUNCHER: "1",
};

refreshNpmIntegrations(integrationsToRefresh);

if (refreshOnly) {
  process.exit(0);
}

const result = spawnSync(binaryPath, process.argv.slice(2), {
  stdio: "inherit",
  env: nativeEnvironment,
});

if (result.error) {
  console.error(`sshx: failed to launch native binary: ${result.error.message}`);
  process.exit(1);
}

process.exit(result.status ?? 0);

function findNpmIntegrationsToRefresh() {
  const integrationRoot =
    process.env.SSHX_INTEGRATIONS_DIR ||
    join(homedir(), ".sshx", "integrations");

  const profiles = [];

  for (const profile of integrationProfiles) {
    const profileRoot = join(integrationRoot, profile);
    const markerPath = join(profileRoot, npmManagedMarker);
    const descriptor = readIntegrationDescriptor(profileRoot);
    const isMarked = existsSync(markerPath);
    const isLegacyNpmIntegration =
      descriptor !== null && pathIsInside(descriptor.driverPath, cacheRoot);

    if (!isMarked && !isLegacyNpmIntegration) {
      continue;
    }
    if (
      isMarked &&
      descriptor !== null &&
      samePath(descriptor.driverPath, binaryPath)
    ) {
      continue;
    }

    profiles.push(profile);
  }

  return profiles;
}

function refreshNpmIntegrations(profiles) {
  for (const profile of profiles) {
    const refresh = spawnSync(binaryPath, ["integrate", "install", profile], {
      encoding: "utf8",
      env: nativeEnvironment,
    });
    if (refresh.status === 0 && !refresh.error) {
      console.error(`sshx: refreshed ${profile} integration for npm ${version}`);
      continue;
    }

    const detail =
      refresh.error?.message ||
      refresh.stderr?.trim() ||
      `native installer exited with status ${refresh.status ?? "unknown"}`;
    console.error(
      `sshx: warning: could not refresh ${profile} integration: ${detail}`,
    );
  }
}

function readIntegrationDescriptor(profileRoot) {
  try {
    const value = JSON.parse(
      readFileSync(join(profileRoot, "integration.json"), "utf8"),
    );
    if (typeof value.driverPath === "string" && value.driverPath.length > 0) {
      return value;
    }
  } catch {
    // A marker without a readable descriptor is repaired by the native installer.
  }
  return null;
}

function samePath(left, right) {
  return resolve(left) === resolve(right);
}

function pathIsInside(candidate, rootPath) {
  const child = relative(resolve(rootPath), resolve(candidate));
  return (
    child !== "" &&
    child !== ".." &&
    !child.startsWith(`..${sep}`) &&
    !isAbsolute(child)
  );
}

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
