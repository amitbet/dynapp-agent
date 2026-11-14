class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.61"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.61/dynapp-shell-agent-0.1.61-darwin-arm64"
      sha256 "3f4dfe225637b9b8d68ba02c3ffb5379cf8396e1f36f03de5d159924a2ba7f8b"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.61/dynapp-shell-agent-0.1.61-darwin-amd64"
      sha256 "23074e69a375f9d4293f4da04648026cb0cc5e9c5a18e7670b68f700d78607da"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.61/dynapp-shell-agent-0.1.61-linux-arm64"
      sha256 "101993db1ab67e1e3d1f069e9e8a82db565d9b0d68044f7931162e42d31711f6"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.61/dynapp-shell-agent-0.1.61-linux-amd64"
      sha256 "f8b3c35d4120f1e5250eda0a0dd143740e38b8194953a34f412f4d193b7c8ba2"
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
