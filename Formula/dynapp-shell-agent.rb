class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.27"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.27/dynapp-shell-agent-0.1.27-darwin-arm64"
      sha256 "532aefa9f3323eb54f5175eb1e8f3bb4b493254ac0c009dfc7188383660f5378"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.27/dynapp-shell-agent-0.1.27-darwin-amd64"
      sha256 "7d8d3489fe78572e98ea7f3f682583d952b345cf21482506617e93085d66efb8"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.27/dynapp-shell-agent-0.1.27-linux-arm64"
      sha256 "8ec33079c9bc6c151ef4879915dcdd0f508b9d425a152f13a17e1c320546a583"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.27/dynapp-shell-agent-0.1.27-linux-amd64"
      sha256 "451f9e8384f002ccb361d8764bdac602c4a5a44837741b033b4534b5f74431f0"
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
