class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.21"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.21/dynapp-shell-agent-0.1.21-darwin-arm64"
      sha256 "0d75c3234fe04947569bb39b68f5867f0eaa951ce77e803bec4f1e54be64441f"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.21/dynapp-shell-agent-0.1.21-darwin-amd64"
      sha256 "95f720bf3a551e141e43c7f74302ecdafbcfc1f85c8534920fc15b6bdbedf4c8"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.21/dynapp-shell-agent-0.1.21-linux-arm64"
      sha256 "4d85c09580ddf92ab36b96f7362b88709366f6c22b046a03c87572a1b0cfb015"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.21/dynapp-shell-agent-0.1.21-linux-amd64"
      sha256 "a5dec78ff3528beaf08b7f99308bf4cc93a1a34dc065e3f218ce6a759fba67d9"
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
