class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.17"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.17/dynapp-shell-agent-0.1.17-darwin-arm64"
      sha256 "3ea7be45979b8b31782b41f2a652477658ffdc8ed7c7852974e5a2457a2b98b4"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.17/dynapp-shell-agent-0.1.17-darwin-amd64"
      sha256 "038d4093d977cef5303e91e9457337a285d709c26aebfc1dffc58dbe064b57e0"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.17/dynapp-shell-agent-0.1.17-linux-arm64"
      sha256 "8b100517931178684f4ad2a9d95feb8b453a5dd8c0ab1bd46ddc084fe446d3bb"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.17/dynapp-shell-agent-0.1.17-linux-amd64"
      sha256 "4f18eac955d674525fdc9b1474413cb6e4defc2db3760c737477a6c810903f2b"
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
