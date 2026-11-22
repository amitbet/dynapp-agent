class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.110"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.110/dynapp-shell-agent-0.1.110-darwin-arm64"
      sha256 "4d9d0a7ca5bfd057d847afee2df0e5e8c3516c8937a4f4d9259fe131c8c26dda"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.110/dynapp-shell-agent-0.1.110-darwin-amd64"
      sha256 "bfe4cdef6b582b4eace1e90ba4ffddd01d208409a7b92a251fe3e7abdb93d6ec"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.110/dynapp-shell-agent-0.1.110-linux-arm64"
      sha256 "5d0e43c305ca5f1df0466ef005dd3d8f4819769fb258ce596095b05ab84ca3ae"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.110/dynapp-shell-agent-0.1.110-linux-amd64"
      sha256 "beb9861826256592a737364a82cb6e22381e149766e7cf4d9d3022ff557163ae"
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
