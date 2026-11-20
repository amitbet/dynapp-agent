class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.100"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.100/dynapp-shell-agent-0.1.100-darwin-arm64"
      sha256 "777ebe859799cb91e6b746d26a1b3209e754a58b009121be5d0a41992db74dac"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.100/dynapp-shell-agent-0.1.100-darwin-amd64"
      sha256 "07b2e09648c7ed0b214611e2c5c568e9bd8f21e9b96e041a7385f87b3b54a922"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.100/dynapp-shell-agent-0.1.100-linux-arm64"
      sha256 "a021dd6a035fd7c4de3146e77996a902ef72f2c0c96d14feac141867e393873a"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.100/dynapp-shell-agent-0.1.100-linux-amd64"
      sha256 "1b0c1468f936d810315ddfe38e9d58692d8370763a59ab2b2a98921a315cb8b8"
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
