#!/usr/bin/env node

import { spawnSync } from "node:child_process";
import * as NodeCrypto from "node:crypto";
import * as NodeFs from "node:fs/promises";
import * as NodeOs from "node:os";
import * as NodePath from "node:path";
import * as NodeUrl from "node:url";

const identity = process.env.MACOS_CODESIGN_IDENTITY || "DynApp Local Signing";
const identifier = process.env.MACOS_CODESIGN_IDENTIFIER || "com.amitbet.dynapp.shell-agent";
const repoRoot = NodePath.resolve(NodePath.dirname(NodeUrl.fileURLToPath(import.meta.url)), "..");
const p12Path = process.env.MACOS_CODESIGN_P12_PATH || NodePath.join(repoRoot, "keys", "macos-codesign.p12");
const passPath = process.env.MACOS_CODESIGN_PASS_PATH || NodePath.join(repoRoot, "keys", "macos-codesign.pass");
const binaryPath = process.argv[2];

if (!binaryPath) throw new Error("Usage: node scripts/sign-macos-binary.mjs <Mach-O binary>");

function run(command, args, options = {}) {
  const result = spawnSync(command, args, { encoding: "utf8", ...options });
  if (result.status !== 0) {
    const detail = [result.stderr, result.stdout].filter(Boolean).join("\n").trim();
    throw new Error(`${command} ${args.join(" ")} failed (exit ${result.status})${detail ? `:\n${detail}` : ""}`);
  }
  return `${result.stdout ?? ""}${result.stderr ?? ""}`;
}

function security(args) {
  return run("security", args);
}

const password = (await NodeFs.readFile(passPath, "utf8")).trim();
const keychainDir = await NodeFs.mkdtemp(NodePath.join(NodeOs.tmpdir(), "dynapp-agent-sign-"));
const keychain = NodePath.join(keychainDir, "signing.keychain-db");
const keychainPassword = NodeCrypto.randomBytes(18).toString("base64url");
const originalSearchList = security(["list-keychains", "-d", "user"])
  .split("\n")
  .map((line) => line.trim().replace(/^"|"$/g, ""))
  .filter(Boolean);
let searchListModified = false;

try {
  security(["create-keychain", "-p", keychainPassword, keychain]);
  security(["unlock-keychain", "-p", keychainPassword, keychain]);
  security(["import", p12Path, "-k", keychain, "-P", password, "-T", "/usr/bin/codesign"]);
  security(["set-key-partition-list", "-S", "apple-tool:,apple:", "-s", "-k", keychainPassword, keychain]);
  security(["list-keychains", "-d", "user", "-s", keychain, ...originalSearchList]);
  searchListModified = true;

  run("codesign", [
    "--force", "--sign", identity, "--keychain", keychain,
    "--identifier", identifier, binaryPath,
  ]);

  const details = run("codesign", ["-dvv", binaryPath]);
  if (/\badhoc\b/i.test(details) || !details.includes(`Authority=${identity}`) || !details.includes(`Identifier=${identifier}`)) {
    throw new Error(`Signing did not produce the expected identity or identifier for ${binaryPath}:\n${details}`);
  }
  run("codesign", ["--verify", "--strict", "--verbose=2", binaryPath]);
  console.log(`Signed ${NodePath.basename(binaryPath)} with ${identity} (${identifier}).`);
} finally {
  if (searchListModified) security(["list-keychains", "-d", "user", "-s", ...originalSearchList]);
  spawnSync("security", ["delete-keychain", keychain]);
  await NodeFs.rm(keychainDir, { recursive: true, force: true });
}
