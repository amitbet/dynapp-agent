class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.3"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.3/dynapp-shell-agent-0.1.3-darwin-arm64"
      sha256 "4f478d475b8df73719a9d44bb1b48dac6ac239bea9c33da82ccca103c74b48f3"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.3/dynapp-shell-agent-0.1.3-darwin-amd64"
      sha256 "ced494e516edc63cb2a9e10628e1025cba724ef4031c9c399b04d30e0d4bd743"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.3/dynapp-shell-agent-0.1.3-linux-arm64"
      sha256 "e4695b902f5e522811423d7572946ed38656758007217a4f61bf85866e7ebb20"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.3/dynapp-shell-agent-0.1.3-linux-amd64"
      sha256 "d6ea47379631872ce65643413d5aabd4745a80bb14484197f23f795b1a847026"
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
