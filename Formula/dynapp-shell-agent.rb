class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.68"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.68/dynapp-shell-agent-0.1.68-darwin-arm64"
      sha256 "7e23892bb57721beb6246f5d60d2b5cfe28e093f4f6cd465561a22fe1a9e48cd"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.68/dynapp-shell-agent-0.1.68-darwin-amd64"
      sha256 "497a23b75e84ce7c050ecddf8d17b8d8b4fd715cb7f9e65568bdbf241c2f5e64"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.68/dynapp-shell-agent-0.1.68-linux-arm64"
      sha256 "08150d41b5185e05d22c1bc0218a1d963f983163ca1907bad5653c8b634a4e90"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.68/dynapp-shell-agent-0.1.68-linux-amd64"
      sha256 "0a529c7e99fe59d7a3296b47f03c50469a0d58da12a2ca2fcf028e9af994923f"
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
