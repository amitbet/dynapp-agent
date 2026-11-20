class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.96"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.96/dynapp-shell-agent-0.1.96-darwin-arm64"
      sha256 "8a3b215cf97f2b77ba10cef62d140526d92bc8362245e08bdb504106bbe3edd6"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.96/dynapp-shell-agent-0.1.96-darwin-amd64"
      sha256 "9bfa24074f7d2fed58b2eaa935bc498d12f206db8bca9474ba850bdbf2c90d2b"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.96/dynapp-shell-agent-0.1.96-linux-arm64"
      sha256 "2f327342e6a8061a38bbadf15ea2a053f191b0a555746310c7c2ce98e0b7fc6b"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.96/dynapp-shell-agent-0.1.96-linux-amd64"
      sha256 "7125293af18557678af6068ebb62f0652bc95ff616c01ae6ba6fd7129896810d"
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
