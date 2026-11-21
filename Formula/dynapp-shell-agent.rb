class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.107"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.107/dynapp-shell-agent-0.1.107-darwin-arm64"
      sha256 "f773b3f3990fa0f4e253710f6553762754ae5f4296cec4fcbea931a5ed6dc4dd"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.107/dynapp-shell-agent-0.1.107-darwin-amd64"
      sha256 "80e157b67c14dc0cff1221870294f404f64d6e7bb1bb0d6a72fd47d255b611d9"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.107/dynapp-shell-agent-0.1.107-linux-arm64"
      sha256 "d34c79534d7fd37490c71cc7846e56dc0b77ae2bc6b2347a17f3e2f7b9ccbda1"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.107/dynapp-shell-agent-0.1.107-linux-amd64"
      sha256 "ea19268eff0e1446fdb868740cd938f88dea87f2b2f1bf8954fe82b2a25d760d"
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
