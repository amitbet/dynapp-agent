class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.63"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.63/dynapp-shell-agent-0.1.63-darwin-arm64"
      sha256 "ab955614b601548af9883d6e3b8e4a029d44b901bf7312a4718f2b055e085e29"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.63/dynapp-shell-agent-0.1.63-darwin-amd64"
      sha256 "51e293b9649a7b0b3aeea5e1416c52cc0c370f39ec4cf3ed1e493d037f312666"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.63/dynapp-shell-agent-0.1.63-linux-arm64"
      sha256 "3ba1a7dfb66c0a8bd5fea00bbe8e4c6e67342fe4d044f8070648aeef7bd5edbf"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.63/dynapp-shell-agent-0.1.63-linux-amd64"
      sha256 "5e87e10ae60e5c8bca579b1f698677b3a0628df23528c88563c4a25d83e66497"
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
