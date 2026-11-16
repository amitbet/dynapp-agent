class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.80"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.80/dynapp-shell-agent-0.1.80-darwin-arm64"
      sha256 "58b3140afbc4ca8862d3bbb1fcff3c01baa41c9f4a0d96c4dda871db2b1f6fea"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.80/dynapp-shell-agent-0.1.80-darwin-amd64"
      sha256 "7fe154d1a6626222231562d9c054d501ed221a1843c694c81695c1e52fc95d1c"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.80/dynapp-shell-agent-0.1.80-linux-arm64"
      sha256 "ad5d482e86d09bf1d20002a006a5e22bbbc75d1144afc78470f5113c5355f95e"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.80/dynapp-shell-agent-0.1.80-linux-amd64"
      sha256 "b4e2b83b76161ce4e4fd34a42d43f19d3b77bc6d73e044553fed16d27ffabaf4"
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
