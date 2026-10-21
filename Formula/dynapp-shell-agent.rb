class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.51"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.51/dynapp-shell-agent-0.1.51-darwin-arm64"
      sha256 "c41e893b1a1fecff133b601f4fa37e7266b4d8510b16ffb65b54744a27405ac4"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.51/dynapp-shell-agent-0.1.51-darwin-amd64"
      sha256 "922737cfbb396fc716b9d7b273b1eb6af212821360b32aaf947d0185b34afab4"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.51/dynapp-shell-agent-0.1.51-linux-arm64"
      sha256 "31964a9b8babd69b4e8ab77d33e65457850d6293990044439b62f8400aeb19c9"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.51/dynapp-shell-agent-0.1.51-linux-amd64"
      sha256 "c369951f07886e43dd87776e649b3b3b476b2d7b4b7b5c6843bf8ff43d7ebe61"
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
