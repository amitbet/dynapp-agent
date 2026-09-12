class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.8"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.8/dynapp-shell-agent-0.1.8-darwin-arm64"
      sha256 "3397a5c22cc0b882558ec8d70055d4ead703b0c731c166aee3bb8040405f49e4"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.8/dynapp-shell-agent-0.1.8-darwin-amd64"
      sha256 "c2d90c73a83eadd0c7443525111792ff72473a9d88a16e2b25da0f84372465f8"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.8/dynapp-shell-agent-0.1.8-linux-arm64"
      sha256 "9bbb4fa7d16e09f3500bf6c8a9fb6a033edc74f4edf981b985ae22d8f9a7aa23"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.8/dynapp-shell-agent-0.1.8-linux-amd64"
      sha256 "71133d3a9f475a81c858b6e208c29bbbad9b277c541562b8969cc2ee1f4a8ae1"
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
