import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import {
  chmodSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

const repositoryRoot = dirname(dirname(fileURLToPath(import.meta.url)));
const launcher = join(repositoryRoot, "bin", "sshx.js");
const packageJson = JSON.parse(
  readFileSync(join(repositoryRoot, "package.json"), "utf8"),
);
const platform = process.platform;
const architecture = process.arch === "x64" ? "amd64" : process.arch;
const binaryName = `sshx-${platform}-${architecture}`;

test("postinstall refreshes a marked integration with the current npm binary", (t) => {
  const fixture = createFixture(t);
  const profileRoot = join(fixture.integrationRoot, "vscode");
  mkdirSync(profileRoot, { recursive: true });
  writeFileSync(join(profileRoot, ".npm-managed"), "1\n");
  writeDescriptor(profileRoot, join(fixture.cacheRoot, "old", binaryName));

  const result = runLauncher(fixture, "--npm-refresh-integrations");

  assert.equal(result.status, 0, result.stderr);
  assert.match(result.stderr, /refreshed vscode integration/);
  assert.deepEqual(readCalls(fixture.logPath), [
    "1|integrate install -y vscode",
  ]);
});

test("launcher adopts an existing npm integration and refreshes before use", (t) => {
  const fixture = createFixture(t);
  const profileRoot = join(fixture.integrationRoot, "cursor");
  mkdirSync(profileRoot, { recursive: true });
  writeDescriptor(profileRoot, join(fixture.cacheRoot, "old", binaryName));

  const result = runLauncher(fixture, "--version");

  assert.equal(result.status, 0, result.stderr);
  assert.deepEqual(readCalls(fixture.logPath), [
    "1|integrate install -y cursor",
    "1|--version",
  ]);
});

test("launcher skips a marked integration that already uses the current binary", (t) => {
  const fixture = createFixture(t);
  const profileRoot = join(fixture.integrationRoot, "vscode");
  mkdirSync(profileRoot, { recursive: true });
  writeFileSync(join(profileRoot, ".npm-managed"), "1\n");
  writeDescriptor(profileRoot, fixture.binaryPath);

  const result = runLauncher(fixture, "--version");

  assert.equal(result.status, 0, result.stderr);
  assert.deepEqual(readCalls(fixture.logPath), ["1|--version"]);
});

function createFixture(t) {
  const root = mkdtempSync(join(tmpdir(), "sshx-npm-refresh-"));
  t.after(() => rmSync(root, { force: true, recursive: true }));
  const cacheRoot = join(root, "cache");
  const integrationRoot = join(root, "integrations");
  const logPath = join(root, "calls.log");
  const binaryPath = join(
    cacheRoot,
    packageJson.version,
    binaryName,
  );
  mkdirSync(dirname(binaryPath), { recursive: true });
  writeFileSync(
    binaryPath,
    "#!/bin/sh\nprintf '%s|%s\\n' \"${SSHX_NPM_LAUNCHER:-}\" \"$*\" >> \"$SSHX_TEST_LOG\"\n",
  );
  chmodSync(binaryPath, 0o755);
  return { binaryPath, cacheRoot, integrationRoot, logPath };
}

function writeDescriptor(profileRoot, driverPath) {
  writeFileSync(
    join(profileRoot, "integration.json"),
    `${JSON.stringify({ driverPath })}\n`,
  );
}

function runLauncher(fixture, argument) {
  return spawnSync(process.execPath, [launcher, argument], {
    encoding: "utf8",
    env: {
      ...process.env,
      SSHX_CACHE_DIR: fixture.cacheRoot,
      SSHX_INTEGRATIONS_DIR: fixture.integrationRoot,
      SSHX_TEST_LOG: fixture.logPath,
    },
  });
}

function readCalls(logPath) {
  return readFileSync(logPath, "utf8").trim().split("\n");
}
