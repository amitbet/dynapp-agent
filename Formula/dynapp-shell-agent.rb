class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.31"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.31/dynapp-shell-agent-0.1.31-darwin-arm64"
      sha256 "17b5800d4d0e09b88e26d2899fbd8b49136f5606bd819da4f97ae5a4c43c1a00"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.31/dynapp-shell-agent-0.1.31-darwin-amd64"
      sha256 "61c72cc9cb4e5585dcbad703a0847007e33c972801853f4f9b1c91ae772dc140"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.31/dynapp-shell-agent-0.1.31-linux-arm64"
      sha256 "f6a2958bdcb6968fd0c4d1d158ed035cb0d64ae9126cee90a45ce5a49ebae43c"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.31/dynapp-shell-agent-0.1.31-linux-amd64"
      sha256 "e804e9359c4ca3ef18b84b2f08596aecc5fda83f1f9c7f70b4ad7fdb97da1ba7"
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
