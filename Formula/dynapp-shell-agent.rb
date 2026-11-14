class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.66"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.66/dynapp-shell-agent-0.1.66-darwin-arm64"
      sha256 "90f57a18ff0461240934c4550d1e3321cad76f9501a5f5e4f9be28c16b5f74bc"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.66/dynapp-shell-agent-0.1.66-darwin-amd64"
      sha256 "3af8d07c88dc48993e3d45423762308c359d060653dedfcdb685653c25656f26"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.66/dynapp-shell-agent-0.1.66-linux-arm64"
      sha256 "ed46e6f55d4ae2d551575c008f8a8d6393b257c34c5149a9c468116efa209541"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.66/dynapp-shell-agent-0.1.66-linux-amd64"
      sha256 "84a7d8118c92ca2af61758e22cde61a8afc39a66318724d0df7b868ea05403c0"
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
