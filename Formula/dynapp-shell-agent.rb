class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.11"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.11/dynapp-shell-agent-0.1.11-darwin-arm64"
      sha256 "8db48ad9ad34a004e08d7e09474734cb9a0f39fc14d986fe932e813b4a16f220"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.11/dynapp-shell-agent-0.1.11-darwin-amd64"
      sha256 "aa920fa076e0e2f8b27cfeb6721ac28e8d8bd80cc19ab28407356567ee5ae683"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.11/dynapp-shell-agent-0.1.11-linux-arm64"
      sha256 "4d1569e6a774d73056e04dbe8ea29f7be3cebd764a432d9a1e1b366ab9dcffbe"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.11/dynapp-shell-agent-0.1.11-linux-amd64"
      sha256 "39ab3cbf4b0bf1ee1953a1cd99416b632ce6bd9a27559d717e903d119567687b"
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
