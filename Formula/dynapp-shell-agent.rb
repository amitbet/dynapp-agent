class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.15"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.15/dynapp-shell-agent-0.1.15-darwin-arm64"
      sha256 "1fb72942d216e34096b2f4e55ec2b5eff4f65e36b6260c08fa7d95998ffb7c66"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.15/dynapp-shell-agent-0.1.15-darwin-amd64"
      sha256 "35a753a123474cbed4052ac4d63dc3ab4c9844794340a72628a439a09f2db030"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.15/dynapp-shell-agent-0.1.15-linux-arm64"
      sha256 "85e1a927c0c68738422f6a9cca0c07c346ed277ab01eab6563efc5518adc5800"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.15/dynapp-shell-agent-0.1.15-linux-amd64"
      sha256 "5b90510ec54e3fc14456e9754b988a8de61b6877080204d8107973ebb631097b"
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
