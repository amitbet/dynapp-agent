class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.19"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.19/dynapp-shell-agent-0.1.19-darwin-arm64"
      sha256 "b402261971b59d4d6245a72b9b3ca1b7545712138b62bb961d7ea445d6703e95"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.19/dynapp-shell-agent-0.1.19-darwin-amd64"
      sha256 "89e43ad18f424620237a8a3732a4468a42ce86ffaf7dbe6fddcc311064442e7e"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.19/dynapp-shell-agent-0.1.19-linux-arm64"
      sha256 "90f241c5d6e16292c5cd66ad8bf1669fae991a460043e352a84c8906496e4ee2"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.19/dynapp-shell-agent-0.1.19-linux-amd64"
      sha256 "498439149ff8aa18a47e499515dd62df243d0f4c5eefc7939480142e31b52299"
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
