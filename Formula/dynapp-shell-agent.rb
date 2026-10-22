class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.52"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.52/dynapp-shell-agent-0.1.52-darwin-arm64"
      sha256 "465cec2a98ddeaba19622fb51a26a6795ebe690603435c7ca0376ff84cf1716b"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.52/dynapp-shell-agent-0.1.52-darwin-amd64"
      sha256 "55d57df235f9be60904355c84090b54bad5a4570db4c86318e9d9dec059a235a"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.52/dynapp-shell-agent-0.1.52-linux-arm64"
      sha256 "23dcf69257a160e8d1fe7476e1de0de3c2cc880099c908ed2b19caef5f1f6d2a"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.52/dynapp-shell-agent-0.1.52-linux-amd64"
      sha256 "165f744c61a020fbec5b22352c3e4a2eca81b395c4894693465d281cbb9f18cd"
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
