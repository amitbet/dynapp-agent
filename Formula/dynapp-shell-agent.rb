class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.29"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.29/dynapp-shell-agent-0.1.29-darwin-arm64"
      sha256 "6cd3f3bf829168471b2b9683af53ab700c1d26ad42f61dcd1971ff83cfe66ca9"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.29/dynapp-shell-agent-0.1.29-darwin-amd64"
      sha256 "a2c5b5db9381381f4e92018b348847d60886589f4b272818c345e40c91132056"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.29/dynapp-shell-agent-0.1.29-linux-arm64"
      sha256 "92b12b8b85a438315c3669449af2cf550420a0b3e5e33b677d70252e4b05ee0b"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.29/dynapp-shell-agent-0.1.29-linux-amd64"
      sha256 "61a094b0088d40316624b6f73ad4ae8a1fa64f3ad388e0fecec2308c260a455c"
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
