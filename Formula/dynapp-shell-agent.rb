class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.69"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.69/dynapp-shell-agent-0.1.69-darwin-arm64"
      sha256 "c3fa69eddde55527809744a7003823cb9776a9c7c900b8f6a41e1f7cf78e242f"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.69/dynapp-shell-agent-0.1.69-darwin-amd64"
      sha256 "0d26d3b37d31eb7c9a8db6c2e762ffe02d1aff305c04a13f97e0d47930b83ff5"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.69/dynapp-shell-agent-0.1.69-linux-arm64"
      sha256 "01f55068616fcdcff77f8ac9dfa7d6c57c9986e27d1c8247e002217e670dbb71"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.69/dynapp-shell-agent-0.1.69-linux-amd64"
      sha256 "9336ef2d141e1e0f052ba6f44af2f729ed16969f4a66709c280dfdf1e783c4a8"
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
