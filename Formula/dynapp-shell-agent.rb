class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.67"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.67/dynapp-shell-agent-0.1.67-darwin-arm64"
      sha256 "b8b164cf882d56e1404d410d79ac8f76428b408964b5341e5875f61eb12b8749"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.67/dynapp-shell-agent-0.1.67-darwin-amd64"
      sha256 "bf3e7e4bd6e2f8f7c2ab70967af8899a22c50ee1588e6ad8f0b951fc34b261d6"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.67/dynapp-shell-agent-0.1.67-linux-arm64"
      sha256 "cfa8b6728dfae25b9bf9384b92474f797364aff71818fd7cbc0fc6cd1b539cb4"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.67/dynapp-shell-agent-0.1.67-linux-amd64"
      sha256 "ce318fcf495280180d4584498da207f93a16675b0ee70d02d9bdedde1cac3d33"
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
