class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.94"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.94/dynapp-shell-agent-0.1.94-darwin-arm64"
      sha256 "23a5ed57fc81da8b420f959fb61c850f669b673841ebcb4e88d138aefcb7d269"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.94/dynapp-shell-agent-0.1.94-darwin-amd64"
      sha256 "56e620deadd479834a24913065e5aacb4401766dd9d5ce21018a0f4e29bab2eb"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.94/dynapp-shell-agent-0.1.94-linux-arm64"
      sha256 "689674bf87f3805efff58524b8eca5cf376d5270871354bed69dfaea0479336d"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.94/dynapp-shell-agent-0.1.94-linux-amd64"
      sha256 "5a9800d25926a9ccc1ebbcc807c881d5666f117d9413375f117cefee88b96de9"
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
