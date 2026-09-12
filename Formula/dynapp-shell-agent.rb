class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.6"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.6/dynapp-shell-agent-0.1.6-darwin-arm64"
      sha256 "b244899065a32886957b7cf2231d2eb0f132de51474947b543bc0c2f277cc78c"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.6/dynapp-shell-agent-0.1.6-darwin-amd64"
      sha256 "726e969d02657781faee03c2637f8cb627a25021add9d3e1995ca624c1bd4bed"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.6/dynapp-shell-agent-0.1.6-linux-arm64"
      sha256 "2a5f930b0c113bdf8ed832bc49bb83c45bb771ed8826c6f79583bfb082102392"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.6/dynapp-shell-agent-0.1.6-linux-amd64"
      sha256 "649262bb396df6c0dbae85ae787373f3c9ab91765bad59f89978244764ce76d4"
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
