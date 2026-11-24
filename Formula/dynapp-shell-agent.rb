class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.112"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.112/dynapp-shell-agent-0.1.112-darwin-arm64"
      sha256 "bc68e55106dfb679cf262e1cd6a352489643575a6a71a37b8f384cf1f277aff7"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.112/dynapp-shell-agent-0.1.112-darwin-amd64"
      sha256 "0fb34617d078ecd92fed5e863eefbb4cf68cc893b54067c238f25dc565576a3b"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.112/dynapp-shell-agent-0.1.112-linux-arm64"
      sha256 "3266b715f9a0ccefb0e1023204c85947e358e074ba3ac9f134b23a842efd10e2"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.112/dynapp-shell-agent-0.1.112-linux-amd64"
      sha256 "45d688cceaa1e1768640f8c4c3ce7a300ddd8a863514d5a51c69b16da0d3a34f"
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
