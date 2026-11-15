class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.74"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.74/dynapp-shell-agent-0.1.74-darwin-arm64"
      sha256 "c7d4ee0b48aa9559e82058678591324004acb04fc2569e8b7f20fb8e482c7ae0"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.74/dynapp-shell-agent-0.1.74-darwin-amd64"
      sha256 "ee2165f5fbd6c82682eb7d22ba1675729959e6daf3d84c1dd0dfc788b3c6cd79"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.74/dynapp-shell-agent-0.1.74-linux-arm64"
      sha256 "fb2a0f7ce14d3d6b8015ebae38445a9e88ad8ec347a79c6358e689e67a91c851"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.74/dynapp-shell-agent-0.1.74-linux-amd64"
      sha256 "fef501754264e57747f301a896d00c5277eb385011546950728ff6bf63afcd80"
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
