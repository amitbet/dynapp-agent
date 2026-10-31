class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.54"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.54/dynapp-shell-agent-0.1.54-darwin-arm64"
      sha256 "544dfffebd2fa1b1672308059b066fa7821a97a4d1e0ad8af1c7a17b72625751"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.54/dynapp-shell-agent-0.1.54-darwin-amd64"
      sha256 "bba7ecb1ea5ff7d727e144b2502176bc6b89b3bd0693f9c3961b00e80cb689b4"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.54/dynapp-shell-agent-0.1.54-linux-arm64"
      sha256 "1be2681e6778c5dbb5f6f36c1a9575d27e75f0d9b2c35ec02614be18962770ed"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.54/dynapp-shell-agent-0.1.54-linux-amd64"
      sha256 "1221cbc4814c08acb30911bfc5ce7d3e96b5017f9b627bd7b06eb7503f6fd686"
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
