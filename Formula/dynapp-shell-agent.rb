class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.65"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.65/dynapp-shell-agent-0.1.65-darwin-arm64"
      sha256 "71ba7824bdddd5f747a6ffc48357c45420d5117358b6fb77dfb2d19a515021e7"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.65/dynapp-shell-agent-0.1.65-darwin-amd64"
      sha256 "be4c936dcf74e16111e577c1658e81526e3338dbede4d60e8a06780c192dbf2e"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.65/dynapp-shell-agent-0.1.65-linux-arm64"
      sha256 "af764822312b1dbe10c8b700a94afa15b3cf62a133ad7bcc6a9965f4c46701d9"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.65/dynapp-shell-agent-0.1.65-linux-amd64"
      sha256 "d59d5237102c90565a8611752c5c19f61b6e8a16218769f4f23205bed8553f05"
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
