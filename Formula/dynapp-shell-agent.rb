class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.76"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.76/dynapp-shell-agent-0.1.76-darwin-arm64"
      sha256 "4406ca6db56e6eeac3c1f96a027ffe86c51f63c2d0f35b30c388769d484f98f3"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.76/dynapp-shell-agent-0.1.76-darwin-amd64"
      sha256 "61c19dc2780aa68a1bff1a6a6c4c2cfc1305190e18d07a41141544953a89c3f7"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.76/dynapp-shell-agent-0.1.76-linux-arm64"
      sha256 "36143ca258c03524ac3cda7fb5da343f9d131a7dbe3b09a6f604c01850fef9a0"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.76/dynapp-shell-agent-0.1.76-linux-amd64"
      sha256 "ad53a84550329d4fa5b68f90966cb2f7a8c38a9f8754099988b9e3606f138ccf"
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
