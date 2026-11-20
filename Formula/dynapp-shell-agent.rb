class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.93"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.93/dynapp-shell-agent-0.1.93-darwin-arm64"
      sha256 "20d946804197bbcaae46e1096b10c91dc49a3a8c3e8a98e25fbde1e5e145a85c"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.93/dynapp-shell-agent-0.1.93-darwin-amd64"
      sha256 "a9f90c494e8ed269b2a2b31590975c8101ea2c420be5649785c61a40379c7926"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.93/dynapp-shell-agent-0.1.93-linux-arm64"
      sha256 "7b5b923bd732868f324470a4f5ca93358597c4a37e1b95c0b72368b471e99fa1"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.93/dynapp-shell-agent-0.1.93-linux-amd64"
      sha256 "49ce16ae67a8cfe0a174291f1e0609ef0dfdcbf775c8305f989122ba29039cc7"
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
