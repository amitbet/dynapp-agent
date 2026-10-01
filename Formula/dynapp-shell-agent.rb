class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.32"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.32/dynapp-shell-agent-0.1.32-darwin-arm64"
      sha256 "2eefc4dc1682d2b04361f71c186fb5c2191743ce98fd3aac5c51f861abbea539"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.32/dynapp-shell-agent-0.1.32-darwin-amd64"
      sha256 "aa493c89ee70c3e5cb0f221f9714fd81affb99c5663cea3c9135c37e718c673e"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.32/dynapp-shell-agent-0.1.32-linux-arm64"
      sha256 "f030be6de62b940c6977d90c34b1e26b3a56b066c82d7ad600c0bb5d844a2dd1"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.32/dynapp-shell-agent-0.1.32-linux-amd64"
      sha256 "27ac8f2abaa063bccc3a611692a553262ffe5628b225d4f95f222f7656de1960"
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
