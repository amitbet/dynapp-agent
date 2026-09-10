class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.1"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.1/dynapp-shell-agent-0.1.1-darwin-arm64"
      sha256 "764902c2400d13c350a6020b58fc2d20d454d21ceedff1863af85cafd3c48ae6"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.1/dynapp-shell-agent-0.1.1-darwin-amd64"
      sha256 "eb1739c12b3ee905ecf0e87407551b608d7cf4bf5acd2099810c9a856a1926fc"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.1/dynapp-shell-agent-0.1.1-linux-arm64"
      sha256 "3b07d21df63a64fc6ceb49e03a9a67f0ccbf34af3633ab6c544b527705eb115b"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.1/dynapp-shell-agent-0.1.1-linux-amd64"
      sha256 "d2f2b75091cbbd9e884417de389e655b3720fa8cccfa204bc9b9943b288d663b"
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
