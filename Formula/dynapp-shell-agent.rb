class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.41"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.41/dynapp-shell-agent-0.1.41-darwin-arm64"
      sha256 "e1961468a9687f6033d811208b09166cd3005c6708dd0a23aec8b557a737801e"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.41/dynapp-shell-agent-0.1.41-darwin-amd64"
      sha256 "b39b26ad349e5698c3fe8c581474c9915da7bdc7ca6f59d63324496b1146e662"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.41/dynapp-shell-agent-0.1.41-linux-arm64"
      sha256 "242be85fe23b42648a43966c2c6f7fd02ff0798e2bc9bf8336d3b8c1cb262df1"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.41/dynapp-shell-agent-0.1.41-linux-amd64"
      sha256 "04bd795a6866e341519e604e91cb7d3acbb5f38e2686353072d7aa3669e80caf"
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
