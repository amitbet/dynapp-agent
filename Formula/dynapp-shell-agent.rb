class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.98"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.98/dynapp-shell-agent-0.1.98-darwin-arm64"
      sha256 "fcc7de038ee71c98f53d08952d522b859757892f105335692db98437815c7153"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.98/dynapp-shell-agent-0.1.98-darwin-amd64"
      sha256 "b4eb1346878304aeaae4ff38b786e2726b0d057ed24c5f8ee51417e098ddfcaa"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.98/dynapp-shell-agent-0.1.98-linux-arm64"
      sha256 "f6e6800d6a451728dc7e28efcf40a9bd81c255d1f473562d3e0a46d5dfc4f4c7"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.98/dynapp-shell-agent-0.1.98-linux-amd64"
      sha256 "25c5bb03d711e63cb3c9ecbbce21ac8274b91eeb9ffc1dfc61a78819c9576635"
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
