class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.85"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.85/dynapp-shell-agent-0.1.85-darwin-arm64"
      sha256 "89e390c1d60d9a09cad590b5ec9b3939aa2ff1e3dbfbd39d32404c34df07481b"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.85/dynapp-shell-agent-0.1.85-darwin-amd64"
      sha256 "8370766ae3c0ecc078807465841cf53db5865d183cf7a04f4790cc3837253a7b"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.85/dynapp-shell-agent-0.1.85-linux-arm64"
      sha256 "784ecb2c870d371987a8ccdca5b28ad0a070d08faf1fea0a32ca8f0681b45753"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.85/dynapp-shell-agent-0.1.85-linux-amd64"
      sha256 "9b9240d5b50068f2c918ed6be5a5015c340ba9a39a64c71b59cbff7bd19b4350"
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
