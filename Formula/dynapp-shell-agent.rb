class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.46"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.46/dynapp-shell-agent-0.1.46-darwin-arm64"
      sha256 "4d783b5671cbe2782d61b5d67ec02e9e1168efa0f94704f7e9b7a433fe05e759"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.46/dynapp-shell-agent-0.1.46-darwin-amd64"
      sha256 "e4fe84b4237357bef7948924188af461e4412e3c38cfe251c5f732b2ce029b2c"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.46/dynapp-shell-agent-0.1.46-linux-arm64"
      sha256 "165997b550e7382b3cb06029730745d58c4a819bdcd070801441840e5ced68a4"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.46/dynapp-shell-agent-0.1.46-linux-amd64"
      sha256 "64df6c5c6200850a5710fc7a2e6177dfb179480f4f25a40e72d137e01d9efeb1"
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
