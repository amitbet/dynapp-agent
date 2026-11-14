class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.59"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.59/dynapp-shell-agent-0.1.59-darwin-arm64"
      sha256 "b48f5dd6b8615ad12eb8868e1c0fed30ebee610d650f1bd5f2f9461680423309"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.59/dynapp-shell-agent-0.1.59-darwin-amd64"
      sha256 "e8ba391037f51b892ddc1c2d2a92982ec0b5a92f347e2d11f7f3d8e2c34c6b26"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.59/dynapp-shell-agent-0.1.59-linux-arm64"
      sha256 "c984819714def2c9b7a2dc92ae55e5f725a3a6ac5d25dee944ac94ba80310728"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.59/dynapp-shell-agent-0.1.59-linux-amd64"
      sha256 "b307b38025e7ac4d737680344316ef1799c033ff2e63faa736f09ea81cc5c008"
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
