class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.92"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.92/dynapp-shell-agent-0.1.92-darwin-arm64"
      sha256 "f1b4563172446c5c5142842ce03698749eb459c721037c3d995d25ad35ed2d4f"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.92/dynapp-shell-agent-0.1.92-darwin-amd64"
      sha256 "8d56a16357bd2e919dcb39eee829697c87e9eb888a3de82f6a314a291f2acaf1"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.92/dynapp-shell-agent-0.1.92-linux-arm64"
      sha256 "8cef9d4cee819fed98875606029c19c44b36f735646cff2a87b73ac0d45ad871"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.92/dynapp-shell-agent-0.1.92-linux-amd64"
      sha256 "ae517616cb266dce36930657c8c1245529285681e01b90bdfc783b65c6d0691b"
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
