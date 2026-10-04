class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.39"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.39/dynapp-shell-agent-0.1.39-darwin-arm64"
      sha256 "63d7254603429c0395e045fe6c34a8bb2f54a5f51d367daf2f8317f25ea07ac3"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.39/dynapp-shell-agent-0.1.39-darwin-amd64"
      sha256 "0dae5d7196889042973eab2a2f879a75280553baeabc071c1bf1594c3c70b8ca"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.39/dynapp-shell-agent-0.1.39-linux-arm64"
      sha256 "e4575a62ed24cedede73c696d7d21fba4404049b1e4b02487b3a05326620e0e8"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.39/dynapp-shell-agent-0.1.39-linux-amd64"
      sha256 "c082f9726a9bb89b094ae44f913aaab07e5dbf14e793e03a701dcef1ea39d527"
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
