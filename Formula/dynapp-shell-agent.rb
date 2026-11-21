class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.103"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.103/dynapp-shell-agent-0.1.103-darwin-arm64"
      sha256 "ec36ab1683c659df24f0d7052b8f263089f65a7058e345087468d22ce3be9224"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.103/dynapp-shell-agent-0.1.103-darwin-amd64"
      sha256 "45b4ee048312363d39de6b9a320b10346bec2860ebdf9de8bd3baff0a4b34eb9"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.103/dynapp-shell-agent-0.1.103-linux-arm64"
      sha256 "9ecc3adacfb50df536a17f76e4452ce145273d511f448029f5574cc8104314c9"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.103/dynapp-shell-agent-0.1.103-linux-amd64"
      sha256 "b5babb3fd3d2b8aa63d8c6b66c3b2f27fe9f5e37aed99692115f6a93f1765916"
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
