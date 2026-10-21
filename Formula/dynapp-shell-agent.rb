class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.49"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.49/dynapp-shell-agent-0.1.49-darwin-arm64"
      sha256 "c3a81e4aec8219831489fa7c01671b9b67d35bd9f8c87d407213b4e984a43d99"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.49/dynapp-shell-agent-0.1.49-darwin-amd64"
      sha256 "ec751237139e53c1e6ed21c909e6327b142e8cd8116d8d92f78555b88840703f"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.49/dynapp-shell-agent-0.1.49-linux-arm64"
      sha256 "7a3cc10d0ea30523b0f738a0a4e820ec1637952b0c83d898efc8a0c7be67616a"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.49/dynapp-shell-agent-0.1.49-linux-amd64"
      sha256 "2756660fb8efc9fbeb9a1df324242c8abba3f050ecc6167e0a65579bf6a1a392"
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
