class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.22"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.22/dynapp-shell-agent-0.1.22-darwin-arm64"
      sha256 "bd55ec8e5d13bfb42190cdb9b553bf51a72b33d5cca6f5ff4f2de4cfdf23abaf"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.22/dynapp-shell-agent-0.1.22-darwin-amd64"
      sha256 "4f9d31aee4a8224970332d7e1cbc2015bbb081917256b390588a3afc26423130"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.22/dynapp-shell-agent-0.1.22-linux-arm64"
      sha256 "fff61ad33649b9051ae4822eb98b83121194ced012e4e1ddcf2f8111a4a37edc"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.22/dynapp-shell-agent-0.1.22-linux-amd64"
      sha256 "3da4c2a0e25e2646cabb0ec09c7cac44edf0d46c6062e8f6ec6e9efaf7ad4107"
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
