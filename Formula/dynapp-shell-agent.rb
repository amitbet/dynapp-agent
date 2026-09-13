class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.12"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.12/dynapp-shell-agent-0.1.12-darwin-arm64"
      sha256 "e5778134cf2217207d95013a4e21a7ad6ec22f2d63a6cbd39dab06783b55ec55"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.12/dynapp-shell-agent-0.1.12-darwin-amd64"
      sha256 "777792727fd0ff643c716da35995c5e604c496dbe77d45eb0a817877f6a886b0"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.12/dynapp-shell-agent-0.1.12-linux-arm64"
      sha256 "44b5c822f7dcf2c2bd030bc06257dac7431e490ca2d0978ae28542855f5784a6"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.12/dynapp-shell-agent-0.1.12-linux-amd64"
      sha256 "cecef7dbf6e70ab77d665577a4b5f0d0073a9e556b15be50e1b929284f6fcd51"
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
