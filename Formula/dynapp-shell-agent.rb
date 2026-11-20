class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.95"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.95/dynapp-shell-agent-0.1.95-darwin-arm64"
      sha256 "48ee4590db3dbdbdb5d569195021e4a89fe8ec2d1fd11e76affa0cdbd1b4090e"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.95/dynapp-shell-agent-0.1.95-darwin-amd64"
      sha256 "c2dd2ce6d921a15521c5924a9a8769105acb0d88bbff290ebad15b139b6d8b71"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.95/dynapp-shell-agent-0.1.95-linux-arm64"
      sha256 "1f171f80c455fc4ac17e66181422586af355f199b465f44ecd3402c61493d080"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.95/dynapp-shell-agent-0.1.95-linux-amd64"
      sha256 "30e47c75ba91f03f94de47ebc2f297af9bbd6635cb31eb1fc30e37a3329d73d5"
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
