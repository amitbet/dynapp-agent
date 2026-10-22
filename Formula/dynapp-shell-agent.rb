class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.53"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.53/dynapp-shell-agent-0.1.53-darwin-arm64"
      sha256 "c2c7dabca2bc8a122734d27954795c4788d267900301a2aee8035b28a7c4617d"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.53/dynapp-shell-agent-0.1.53-darwin-amd64"
      sha256 "94bb31ada5feca539bf9cf9d8f4d3b2ee7a6d7244acba228aaa2663212080658"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.53/dynapp-shell-agent-0.1.53-linux-arm64"
      sha256 "c1f6ef5d392b2d02d11dbac344fc02a16bbd964c670604ecd26e0fc7aa78db2e"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.53/dynapp-shell-agent-0.1.53-linux-amd64"
      sha256 "547af4be8b0c728c3e562f7b2b5d2cb8eddb8e6b3163f2c1a96d9ff3d2158525"
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
