class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.14"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.14/dynapp-shell-agent-0.1.14-darwin-arm64"
      sha256 "bce2668f312142614948bcade97bdc0c24e0dd188ed921fe736228d23ea7e577"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.14/dynapp-shell-agent-0.1.14-darwin-amd64"
      sha256 "44d50e9a5da2590c44c1b21565570f328ab6ccabf5e43b829e3af1d13499614c"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.14/dynapp-shell-agent-0.1.14-linux-arm64"
      sha256 "a27a596d3c545174831b285b0a668c21a3dbe5bf9a64ee848360e3109df97c91"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.14/dynapp-shell-agent-0.1.14-linux-amd64"
      sha256 "23ca61c7fd02ecedf9e98f9ec59c725c9f466a56f3e30ffdfec3d9aabdad750b"
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
