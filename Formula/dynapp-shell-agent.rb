class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.111"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.111/dynapp-shell-agent-0.1.111-darwin-arm64"
      sha256 "a41dca6b4b338caed0b7238ed42c64feff65bffdf4f42f7776f788a8ca4f88e0"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.111/dynapp-shell-agent-0.1.111-darwin-amd64"
      sha256 "cdc1a9d38e1b3f1f97abbc65ab4aa1e6ace42549449db763a9a7fac60f4c1365"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.111/dynapp-shell-agent-0.1.111-linux-arm64"
      sha256 "7fbc00c41bf157a07e70a8c9cff13b1941bffa93d6d735cf2a6bd57648a5ab29"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.111/dynapp-shell-agent-0.1.111-linux-amd64"
      sha256 "52248b25429636ae31a858ac6460de6d54889c09faca391ace90d4443587ccbf"
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
