// @ts-check
import { mkdirSync, readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";

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
  console.error(`Unsupported platform ${process.platform}/${process.arch}`);
  process.exit(1);
}

const root = dirname(dirname(fileURLToPath(import.meta.url)));
const outDir = join(root, "dist", "native");
const binary = `sshx-${platform}-${arch}${platform === "windows" ? ".exe" : ""}`;
const packageJson = JSON.parse(readFileSync(join(root, "package.json"), "utf8"));
const version = packageJson.version;

mkdirSync(outDir, { recursive: true });

const result = spawnSync(
  "go",
  [
    "build",
    "-trimpath",
    "-ldflags",
    `-X github.com/xiaot623/sshx/internal/version.Version=${version}`,
    "-o",
    join(outDir, binary),
    "./cmd/sshx",
  ],
  {
    cwd: root,
    stdio: "inherit",
    env: {
      ...process.env,
      GOOS: platform,
      GOARCH: arch,
      CGO_ENABLED: "0",
    },
  },
);

if (result.error) {
  console.error(result.error.message);
  process.exit(1);
}

process.exit(result.status ?? 0);
