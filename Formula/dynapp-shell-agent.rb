class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.75"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.75/dynapp-shell-agent-0.1.75-darwin-arm64"
      sha256 "06519a4b12f08bd3947310d0ff0b5c9edcaef09ae84691daf7d218028c8a87b1"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.75/dynapp-shell-agent-0.1.75-darwin-amd64"
      sha256 "582c6373b2b1a633594bd903680856bed25d780c60f7e5cd7dd7eea7050e31ef"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.75/dynapp-shell-agent-0.1.75-linux-arm64"
      sha256 "93d7dfb8aa519999e29934decf84f3403afd45b742149cb4a1283b780aa85c60"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.75/dynapp-shell-agent-0.1.75-linux-amd64"
      sha256 "56152c7df7019d04d2de634dca010ed395e11eef5a6b8217cbe59f23f6c8ff25"
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
