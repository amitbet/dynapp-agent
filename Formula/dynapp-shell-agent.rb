class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.5"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.5/dynapp-shell-agent-0.1.5-darwin-arm64"
      sha256 "4a80617493c9bd2f2c0d5c7f255a23523cd5e6385bfaa60bc2fd4dea9ed2719a"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.5/dynapp-shell-agent-0.1.5-darwin-amd64"
      sha256 "8dfaddf9662ccdbf1956c4071d5eb595928c2fa6b0ca1a729e0dd0dfa59679d2"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.5/dynapp-shell-agent-0.1.5-linux-arm64"
      sha256 "5ddd065421fe3fb0c9e2fa77136d5971fb98228961b83add0b31c8c0125b9c4c"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.5/dynapp-shell-agent-0.1.5-linux-amd64"
      sha256 "26c45efdf9aad18e3099f06f5eb15ca6ef5accc76b6b7f7452669b7054e4ff1b"
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
