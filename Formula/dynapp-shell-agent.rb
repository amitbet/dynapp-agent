class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.42"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.42/dynapp-shell-agent-0.1.42-darwin-arm64"
      sha256 "e43d79b4854c17e80eae322c02f791b11bb881386cab2b675b89eb6eb86e9c3a"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.42/dynapp-shell-agent-0.1.42-darwin-amd64"
      sha256 "0d141f079ee05be58e28a4034d39da8e20863b22b037a8fa8be8a6370aad569d"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.42/dynapp-shell-agent-0.1.42-linux-arm64"
      sha256 "359e42c86a4b78f9377056d845760d46e7a127da50a3bc5aa04ddf3f6c6f8fda"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.42/dynapp-shell-agent-0.1.42-linux-amd64"
      sha256 "35bab7645bd8bd0e4297558d706a54d896558391c3424483d466b7ad4fd64a46"
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
