class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.109"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.109/dynapp-shell-agent-0.1.109-darwin-arm64"
      sha256 "8fef6d1cf287516f75120eba006511b6749d2bc4df18fac116d30ddc92bade67"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.109/dynapp-shell-agent-0.1.109-darwin-amd64"
      sha256 "6cb93d9403bceef36efb0de14ca21c232e69bc3ab0f5b98399dcd44f9c4a4419"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.109/dynapp-shell-agent-0.1.109-linux-arm64"
      sha256 "ce289f09b6da2ce44f4aaa36d46cf5726d69edcada40affa833c91fe6ea8bb09"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.109/dynapp-shell-agent-0.1.109-linux-amd64"
      sha256 "8e5c5505cb022ccafa592d12da092a6f58289cb078dab8302055dbd8e8d050bc"
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
