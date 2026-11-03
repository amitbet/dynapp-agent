class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.55"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.55/dynapp-shell-agent-0.1.55-darwin-arm64"
      sha256 "57a0e9efe927a37e7a802a88f2587d21ce639880f7ca00f491d1984e4e4061d5"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.55/dynapp-shell-agent-0.1.55-darwin-amd64"
      sha256 "a7c133c7dfac25ffc38364c80509463aa7f9ec61afda5e901b440b116f8fc72d"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.55/dynapp-shell-agent-0.1.55-linux-arm64"
      sha256 "8749c4bd55cc26bcb2b6a9d9809b809f57b42fcd6dfb21580d54ee11d1abccd2"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.55/dynapp-shell-agent-0.1.55-linux-amd64"
      sha256 "3eac29a97517250e83cd245b7dc501c7ed8cfb8701348ad28e125a703418b2bf"
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
