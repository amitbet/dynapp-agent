class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.88"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.88/dynapp-shell-agent-0.1.88-darwin-arm64"
      sha256 "41538300051a18ff24bdf114386d567170e3169461ee8b1503658de747c07104"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.88/dynapp-shell-agent-0.1.88-darwin-amd64"
      sha256 "8f498555420ec801b62bc828abe2c95eb6b32aad31d3a2aa4c346fd721926032"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.88/dynapp-shell-agent-0.1.88-linux-arm64"
      sha256 "81e114235c63b1b1ad5d035b5c730df737083db5ecb1b864f4ac3d3d228f1758"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.88/dynapp-shell-agent-0.1.88-linux-amd64"
      sha256 "88eafc1f14824da07d215af9448b414ae0cfc07ef987e86c445914e2fa6575a8"
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
