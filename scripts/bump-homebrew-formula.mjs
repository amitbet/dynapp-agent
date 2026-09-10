#!/usr/bin/env node

import * as NodeFs from "node:fs";
import * as NodePath from "node:path";
import * as NodeUrl from "node:url";

const version = process.argv[2]?.replace(/^v/, "");
const shaDir = process.argv[3];
if (!version || !shaDir) {
  throw new Error("Usage: node scripts/bump-homebrew-formula.mjs <version> <sha256-dir>");
}

const repoRoot = NodePath.resolve(NodePath.dirname(NodeUrl.fileURLToPath(import.meta.url)), "..");

function sha256For(suffix) {
  const name = `dynapp-shell-agent-${version}-${suffix}.sha256`;
  const text = NodeFs.readFileSync(NodePath.join(shaDir, name), "utf8").trim().split(/\s+/)[0];
  if (!/^[0-9a-f]{64}$/i.test(text)) throw new Error(`invalid checksum in ${name}`);
  return text.toLowerCase();
}

const hashes = {
  "darwin-arm64": sha256For("darwin-arm64"),
  "darwin-amd64": sha256For("darwin-amd64"),
  "linux-arm64": sha256For("linux-arm64"),
  "linux-amd64": sha256For("linux-amd64"),
};

const formula = `class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "${version}"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v${version}/dynapp-shell-agent-${version}-darwin-arm64"
      sha256 "${hashes["darwin-arm64"]}"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v${version}/dynapp-shell-agent-${version}-darwin-amd64"
      sha256 "${hashes["darwin-amd64"]}"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v${version}/dynapp-shell-agent-${version}-linux-arm64"
      sha256 "${hashes["linux-arm64"]}"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v${version}/dynapp-shell-agent-${version}-linux-amd64"
      sha256 "${hashes["linux-amd64"]}"
    end
  end

  def install
    bin.install Dir["dynapp-shell-agent*"].first => "dynapp-shell-agent"
  end

  def caveats
    <<~EOS
      Install and start the OS service with:
        dynapp-shell-agent install
        dynapp-shell-agent start
    EOS
  end

  test do
    assert_predicate bin/"dynapp-shell-agent", :executable?
  end
end
`;

const destination = NodePath.join(repoRoot, "Formula", "dynapp-shell-agent.rb");
NodeFs.mkdirSync(NodePath.dirname(destination), { recursive: true });
NodeFs.writeFileSync(destination, formula);
console.log(`updated ${destination} to ${version}`);
