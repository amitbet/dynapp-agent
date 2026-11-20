class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.101"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.101/dynapp-shell-agent-0.1.101-darwin-arm64"
      sha256 "934aaeab2708c316669260cadf6b17dfd17cd428afac7994ce0af36fc02e0930"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.101/dynapp-shell-agent-0.1.101-darwin-amd64"
      sha256 "d2fbd170bef5ea818ca80d50e9cf490bf2a59aa818b9b3c50fa398b784ffb4ba"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.101/dynapp-shell-agent-0.1.101-linux-arm64"
      sha256 "08801afa2fc22286361c501b7e26634c9eea9e9febfde679da4711de1f12899c"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.101/dynapp-shell-agent-0.1.101-linux-amd64"
      sha256 "ec11a5eb3a0d2412dfdb2e83f1b3f3b04464a4482815eb16830f9c51601c27ab"
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
