class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.10"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.10/dynapp-shell-agent-0.1.10-darwin-arm64"
      sha256 "ee93f3d4adcc43dcac2420433f2c1c52e1be7ba57d17c7095f5d2ea1c498a405"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.10/dynapp-shell-agent-0.1.10-darwin-amd64"
      sha256 "d2f50f712503a3a4f6359636b61f313f9f42703ac3e4f34b7bdf66f6a80cc9e5"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.10/dynapp-shell-agent-0.1.10-linux-arm64"
      sha256 "e1eef8d05f54cd808aac5d807f112e6407eaed9e8a6bca28ef99ac4449f90778"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.10/dynapp-shell-agent-0.1.10-linux-amd64"
      sha256 "3989a65716b5276276e755fe239efcda82176d53d9abb66a86b5f013c32bbda6"
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
