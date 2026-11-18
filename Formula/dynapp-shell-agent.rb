class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.84"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.84/dynapp-shell-agent-0.1.84-darwin-arm64"
      sha256 "ebdd0cdeade66490b33060f44845e95cb1a28f62f23fb7f3c31a2dd5d3876e44"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.84/dynapp-shell-agent-0.1.84-darwin-amd64"
      sha256 "13ce3959b5d7d13e339d8bc816c898ae54bde0fecec8a6c190fdf6fc9cda0bf8"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.84/dynapp-shell-agent-0.1.84-linux-arm64"
      sha256 "b529431b1c8124bc24f6e3b13c585d1ed1b469fbf9b57fd694d4ca4bc18458db"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.84/dynapp-shell-agent-0.1.84-linux-amd64"
      sha256 "1588f71bb78c87c86c5d1e0952ce92b04dd29b1c31e10ed370d3db61d3ce6c9b"
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
