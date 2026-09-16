class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.20"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.20/dynapp-shell-agent-0.1.20-darwin-arm64"
      sha256 "c4ea0285f45162117e44d152dc736b108508fc003e277b9927e69d613defc122"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.20/dynapp-shell-agent-0.1.20-darwin-amd64"
      sha256 "a7af67c43c39bc1b4cec862a4b2e1df66f971c9f74dd02fc077ee35b6dec352b"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.20/dynapp-shell-agent-0.1.20-linux-arm64"
      sha256 "9e927054d890b68632e336115b06abe43720bdf9509f297ae84bee1b18a85bf4"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.20/dynapp-shell-agent-0.1.20-linux-amd64"
      sha256 "8dadc1221ac3b60fb64d7ebe42d58551613e83b83b4bc7713298c4a665014d36"
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
