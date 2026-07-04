// @ts-check
import { execSync } from "node:child_process";

const type = process.argv.slice(2).find((arg) => !arg.startsWith("--"));
const validTypes = ["patch", "minor", "major"];

if (!type || !validTypes.includes(type)) {
  console.error("\nError: Please specify a valid release type (patch, minor, or major)");
  console.error("\nUsage:");
  console.error("  npm run release:patch  # 0.0.1 -> 0.0.2");
  console.error("  npm run release:minor  # 0.0.1 -> 0.1.0");
  console.error("  npm run release:major  # 0.0.1 -> 1.0.0\n");
  process.exit(1);
}

try {
  const status = execSync("git status --porcelain").toString().trim();
  if (status) {
    console.error("\nError: Working directory is not clean. Please commit or stash changes before releasing.");
    process.exit(1);
  }

  console.log(`\nBumping ${type} version...`);
  execSync(`npm version ${type} -m "chore: release v%s"`, { stdio: "inherit" });

  const branch = execSync("git rev-parse --abbrev-ref HEAD").toString().trim();

  console.log(`\nPushing to origin ${branch} with tags...`);
  execSync(`git push origin ${branch} --follow-tags`, { stdio: "inherit" });

  console.log("\nRelease tag pushed. GitHub Actions will build binaries and create the release.");
} catch (error) {
  console.error("\nRelease process failed.", error instanceof Error ? error.message : error);
  process.exit(1);
}
