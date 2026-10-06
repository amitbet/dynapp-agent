class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.40"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.40/dynapp-shell-agent-0.1.40-darwin-arm64"
      sha256 "58198cf4fce52faeadfe55e52a6a42e595a1916700a3ce463db3ce64b29f440f"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.40/dynapp-shell-agent-0.1.40-darwin-amd64"
      sha256 "e023c106a09e6665d1236ca37f7e008f594bb2cc12a34da295d38009dae37a7b"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.40/dynapp-shell-agent-0.1.40-linux-arm64"
      sha256 "a15b04b98ed58a04d67f27fc9b39d609dd77e1f528fdd55f19d98fd154fc515c"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.40/dynapp-shell-agent-0.1.40-linux-amd64"
      sha256 "11408bc51eff9b808f963fd9d74a514386e83ce29e5b6ce82ca25637990672d4"
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
