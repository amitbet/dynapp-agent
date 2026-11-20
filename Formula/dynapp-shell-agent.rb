class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.99"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.99/dynapp-shell-agent-0.1.99-darwin-arm64"
      sha256 "332c06d34c5734b90f4b05598d8baa8ba0be6851972c4d01f8b2bc0e7b8ca4ec"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.99/dynapp-shell-agent-0.1.99-darwin-amd64"
      sha256 "09cb99aed819ad7167bbbfdff890c04832c31d751c522bdc35ae6e1f43d7bb02"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.99/dynapp-shell-agent-0.1.99-linux-arm64"
      sha256 "5de085b547c0a222c33148aa5f185e7699ca622ce9149bc045885102fa5ba5bc"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.99/dynapp-shell-agent-0.1.99-linux-amd64"
      sha256 "05a3d65f9e4f91bb0ffb1f87b4092fb87a7d656aa5a89a1c8cd10920c182fe09"
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
