class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.108"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.108/dynapp-shell-agent-0.1.108-darwin-arm64"
      sha256 "29a2916bf850b2b084f13e00940e94d81934887adaa11c0bf2e9027f36bbf00e"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.108/dynapp-shell-agent-0.1.108-darwin-amd64"
      sha256 "5cd1128126552ddd9e0027520da954b74d5493b4411397aed08498a44d75f9ca"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.108/dynapp-shell-agent-0.1.108-linux-arm64"
      sha256 "00baac7e3da5caf488136a9f87e8182f3370b37499ea1e9ed8b8885728a0a7ea"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.108/dynapp-shell-agent-0.1.108-linux-amd64"
      sha256 "215ab7793bc30a592135d0b9b6fc498c278a783b16bfc65bd57e9a67dd266ff1"
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
