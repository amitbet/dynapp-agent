class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.91"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.91/dynapp-shell-agent-0.1.91-darwin-arm64"
      sha256 "6ecda200ff6ad02cd8bce2314dc6ee4b06e9bc309da6bad3ca1e88c2df45cb5a"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.91/dynapp-shell-agent-0.1.91-darwin-amd64"
      sha256 "67ec8453139489d6a3eec9eabd7b4c252fb4ce01118475706e3997683a6a19ad"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.91/dynapp-shell-agent-0.1.91-linux-arm64"
      sha256 "acfdb851ab460dcf797364f67aeb3163ae7be4e9f8bda990d573d875c50288ca"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.91/dynapp-shell-agent-0.1.91-linux-amd64"
      sha256 "1cd6a9e2c767f75d809523eb6ccc9afc72666ce99fe3b01096ce840e12fe19ae"
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
