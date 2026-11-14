class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.62"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.62/dynapp-shell-agent-0.1.62-darwin-arm64"
      sha256 "f876ce34e2a763d3612b00a6e07746927b5efdf9a1f25b340aab2544f6e93111"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.62/dynapp-shell-agent-0.1.62-darwin-amd64"
      sha256 "ab0464e2ebc34ef27001caa650727a7319a88a161997871bef89e6cced8aa661"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.62/dynapp-shell-agent-0.1.62-linux-arm64"
      sha256 "7be76409e2f84fa668e2a6c89c4008808740676fb18c33e3d6896aac1f7f0b83"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.62/dynapp-shell-agent-0.1.62-linux-amd64"
      sha256 "33e7b47135baf689dd2294ebd466a82ba559e387bfd35bdbcb3e874520cbbd4c"
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
