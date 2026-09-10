#!/usr/bin/env node

import * as NodeFs from "node:fs";
import * as NodePath from "node:path";
import * as NodeUrl from "node:url";

const version = process.argv[2]?.replace(/^v/, "");
const shaDir = process.argv[3];
if (!version || !shaDir) {
  throw new Error("Usage: node scripts/bump-winget-manifest.mjs <version> <sha256-dir>");
}

const repoRoot = NodePath.resolve(NodePath.dirname(NodeUrl.fileURLToPath(import.meta.url)), "..");
const identifier = "AmitBet.DynAppShellAgent";
const manifestVersion = "1.6.0";

function sha256For(suffix) {
  const name = `dynapp-shell-agent-${version}-${suffix}.sha256`;
  const text = NodeFs.readFileSync(NodePath.join(shaDir, name), "utf8").trim().split(/\s+/)[0];
  if (!/^[0-9a-f]{64}$/i.test(text)) throw new Error(`invalid checksum in ${name}`);
  return text.toUpperCase();
}

const hashes = {
  "windows-amd64": sha256For("windows-amd64.exe"),
  "windows-arm64": sha256For("windows-arm64.exe"),
};

const installer = `# yaml-language-server: $schema=https://aka.ms/winget-manifest.installer.1.6.0.schema.json
PackageIdentifier: ${identifier}
PackageVersion: ${version}
InstallerType: portable
Commands:
  - dynapp-shell-agent
UpgradeBehavior: install
Installers:
  - Architecture: x64
    Scope: user
    InstallerUrl: https://github.com/amitbet/dynapp-agent/releases/download/v${version}/dynapp-shell-agent-${version}-windows-amd64.exe
    InstallerSha256: ${hashes["windows-amd64"]}
  - Architecture: x64
    Scope: machine
    InstallerUrl: https://github.com/amitbet/dynapp-agent/releases/download/v${version}/dynapp-shell-agent-${version}-windows-amd64.exe
    InstallerSha256: ${hashes["windows-amd64"]}
  - Architecture: arm64
    Scope: user
    InstallerUrl: https://github.com/amitbet/dynapp-agent/releases/download/v${version}/dynapp-shell-agent-${version}-windows-arm64.exe
    InstallerSha256: ${hashes["windows-arm64"]}
  - Architecture: arm64
    Scope: machine
    InstallerUrl: https://github.com/amitbet/dynapp-agent/releases/download/v${version}/dynapp-shell-agent-${version}-windows-arm64.exe
    InstallerSha256: ${hashes["windows-arm64"]}
ManifestType: installer
ManifestVersion: ${manifestVersion}
`;

const locale = `# yaml-language-server: $schema=https://aka.ms/winget-manifest.defaultLocale.1.6.0.schema.json
PackageIdentifier: ${identifier}
PackageVersion: ${version}
PackageLocale: en-US
Publisher: Amit Bet
PublisherUrl: https://github.com/amitbet
PublisherSupportUrl: https://github.com/amitbet/dynapp-agent/issues
PackageName: DynApp Agent
PackageUrl: https://github.com/amitbet/dynapp-agent
License: All Rights Reserved
LicenseUrl: https://github.com/amitbet/dynapp-agent
ShortDescription: Local agent so DynApps can use files, network, and processes
Moniker: dynapp-shell-agent
Tags:
  - dynapp
  - agent
ManifestType: defaultLocale
ManifestVersion: ${manifestVersion}
`;

const versionManifest = `# yaml-language-server: $schema=https://aka.ms/winget-manifest.version.1.6.0.schema.json
PackageIdentifier: ${identifier}
PackageVersion: ${version}
DefaultLocale: en-US
ManifestType: version
ManifestVersion: ${manifestVersion}
`;

const destination = NodePath.join(repoRoot, "winget");
NodeFs.mkdirSync(destination, { recursive: true });
NodeFs.writeFileSync(NodePath.join(destination, `${identifier}.installer.yaml`), installer);
NodeFs.writeFileSync(NodePath.join(destination, `${identifier}.locale.en-US.yaml`), locale);
NodeFs.writeFileSync(NodePath.join(destination, `${identifier}.yaml`), versionManifest);
console.log(`updated ${destination} to ${version}`);
