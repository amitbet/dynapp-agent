class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.50"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.50/dynapp-shell-agent-0.1.50-darwin-arm64"
      sha256 "c5c0fba2ab8248a5408efe198c1a3db899768404a0689803e624a3df3a90c38f"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.50/dynapp-shell-agent-0.1.50-darwin-amd64"
      sha256 "2d78a0b60addcb4332c572add00533afa2cb0fed216dbb053e8d57628236b6c7"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.50/dynapp-shell-agent-0.1.50-linux-arm64"
      sha256 "5ff2633ca40095810e8c8286d169058cd3d3e84819e3d1d5d1b805848f0de975"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.50/dynapp-shell-agent-0.1.50-linux-amd64"
      sha256 "61a22efbcb27f7daa7d8323a88474e5374dfe6a3e458a6a76bda11437b9f9d5d"
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
