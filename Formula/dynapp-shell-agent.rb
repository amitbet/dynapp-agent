class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.77"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.77/dynapp-shell-agent-0.1.77-darwin-arm64"
      sha256 "3f22f42d707896b45155283c0f2516b7c1e6a65d637a869c0063aa2cbe48470e"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.77/dynapp-shell-agent-0.1.77-darwin-amd64"
      sha256 "0e64b2f4f26ca662861353afeeac4fbd5d75fdf7f49c15b31fc65c65c374ee1b"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.77/dynapp-shell-agent-0.1.77-linux-arm64"
      sha256 "87ceb3518d4b60e6a2dd1be1911cc82814c96d567f37032a943145401c1abb2d"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.77/dynapp-shell-agent-0.1.77-linux-amd64"
      sha256 "fec54d41ce2c5110caf352f345deaf74068b2ac46437ed2afdfce26efe8d0c2b"
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
