class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.18"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.18/dynapp-shell-agent-0.1.18-darwin-arm64"
      sha256 "a46713c8317f0932a18941733d122403cc14e77e02408cc5a587df91025981e9"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.18/dynapp-shell-agent-0.1.18-darwin-amd64"
      sha256 "bc9af883ad106e05f4b501374615c36828b15f79050b47ae65e33011430ce9ff"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.18/dynapp-shell-agent-0.1.18-linux-arm64"
      sha256 "aec5b14313404b9b6d1ff09aae9fb0a6de111f310b354fae106d2ade52c02956"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.18/dynapp-shell-agent-0.1.18-linux-amd64"
      sha256 "a9f61bffd0c1441ccaf381781688e2f1dcdd1543c517db53a9f266342ceb7480"
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
