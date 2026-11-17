class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.83"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.83/dynapp-shell-agent-0.1.83-darwin-arm64"
      sha256 "b8057a2d607fb44b567bdecf1f972f7d27cbc8f88dc2a1f3ef5ebf0e5d77e114"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.83/dynapp-shell-agent-0.1.83-darwin-amd64"
      sha256 "4c99ebe410bef173673e26d9be7ee3adb0504ba76a0d2cc45956f599f9c1605e"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.83/dynapp-shell-agent-0.1.83-linux-arm64"
      sha256 "77000d693ef192cb5af7a31ff6638cc50b01d266cef2aa58714c71c45c5969db"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.83/dynapp-shell-agent-0.1.83-linux-amd64"
      sha256 "0f26f771e7676dfa352272a3c14fa6bc6b859964e4051f6b1a82b54e68569f7c"
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
