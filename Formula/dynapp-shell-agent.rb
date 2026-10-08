class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.47"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.47/dynapp-shell-agent-0.1.47-darwin-arm64"
      sha256 "04c84f71500d6b1da0051b73c32a2b6da0a94304be2923189a3db22aef78d004"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.47/dynapp-shell-agent-0.1.47-darwin-amd64"
      sha256 "7afee543ece01a5b844881a369750529dac0fde03dab210e66bde4e273877fe2"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.47/dynapp-shell-agent-0.1.47-linux-arm64"
      sha256 "e81314097b7061abd57ae164eb42d956d793a4cc50f6697562e65998aae12833"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.47/dynapp-shell-agent-0.1.47-linux-amd64"
      sha256 "55051105d37e5fa789861180582fd4c977940cdf01a3a91b2826f515738b07bf"
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
