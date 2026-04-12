/**
 * Tag and push a release.
 *
 * Usage:
 *   bun run release patch   # 0.0.1 → 0.0.2
 *   bun run release minor   # 0.0.1 → 0.1.0
 *   bun run release major   # 0.0.1 → 1.0.0
 *   bun run release 0.3.0   # explicit version
 *
 * Guards:
 *   - Working tree must be clean (no uncommitted changes)
 *   - Must be on the main branch
 *   - Tag must not already exist
 *
 * The actual build + GitHub Release is handled by the release.yml
 * workflow that triggers on the v* tag push.
 */

import { $ } from "bun";

const MANIFEST = "io.github.anthonybaldwin.UsageButtons.sdPlugin/manifest.json";

// ── Guards ─────────────────────────────────────────────────────

const status = (await $`git status --porcelain`.text()).trim();
if (status) {
  console.error("✗ working tree is dirty — commit or stash first:\n" + status);
  process.exit(1);
}

const branch = (await $`git rev-parse --abbrev-ref HEAD`.text()).trim();
if (branch !== "main") {
  console.error(`✗ releases must be cut from main (currently on ${branch})`);
  process.exit(1);
}

// Make sure we're up to date with remote
await $`git fetch origin main --quiet`;
const local = (await $`git rev-parse HEAD`.text()).trim();
const remote = (await $`git rev-parse origin/main`.text()).trim();
if (local !== remote) {
  console.error("✗ local main is not up to date with origin — pull first");
  process.exit(1);
}

// ── Version resolution ─────────────────────────────────────────

const manifest = await Bun.file(MANIFEST).json();
const current = manifest.Version.split(".").slice(0, 3).map(Number); // [major, minor, patch]

const arg = process.argv[2];
if (!arg) {
  console.error("Usage: bun run release <patch|minor|major|x.y.z>");
  process.exit(1);
}

let next: number[];
if (arg === "patch") {
  next = [current[0], current[1], current[2] + 1];
} else if (arg === "minor") {
  next = [current[0], current[1] + 1, 0];
} else if (arg === "major") {
  next = [current[0] + 1, 0, 0];
} else if (/^\d+\.\d+\.\d+$/.test(arg)) {
  next = arg.split(".").map(Number);
} else {
  console.error(`✗ invalid version argument: ${arg}`);
  console.error("Usage: bun run release <patch|minor|major|x.y.z>");
  process.exit(1);
}

const semver = next.join(".");
const tag = `v${semver}`;

// Check tag doesn't already exist
const existingTags = (await $`git tag -l ${tag}`.text()).trim();
if (existingTags) {
  console.error(`✗ tag ${tag} already exists`);
  process.exit(1);
}

// ── Bump + tag + push ──────────────────────────────────────────

// Update manifest.json version (4-part format for Stream Deck)
manifest.Version = `${semver}.0`;
await Bun.write(MANIFEST, JSON.stringify(manifest, null, 2) + "\n");

// Update package.json version
await $`npm pkg set version=${semver}`.quiet();

// Commit the version bump, tag it, push both
await $`git add ${MANIFEST} package.json`;
await $`git commit -m ${"chore(release): " + semver}`;
await $`git tag -a ${tag} -m ${"Release " + semver}`;
await $`git push origin main ${tag}`;

console.log(`\n✓ ${tag} pushed — release.yml will build and publish the GitHub Release`);
