class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.71"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.71/dynapp-shell-agent-0.1.71-darwin-arm64"
      sha256 "3ba265172924e9c01671bcd1b052d0896dcf5772e10881fbf5372ab15c2ac8f4"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.71/dynapp-shell-agent-0.1.71-darwin-amd64"
      sha256 "b3b045c623d6c918ccf60078b60971346588171c7d6ab197d84596c8112ff031"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.71/dynapp-shell-agent-0.1.71-linux-arm64"
      sha256 "24aea1205804c7e22aa5b85631a40409d52a11b8dd7a887e64fd7545b2693cd7"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.71/dynapp-shell-agent-0.1.71-linux-amd64"
      sha256 "9c13014ecc69aa4f99b5ab7d05e16481ae1a0f0c56a9979ddf775eb67bce20ac"
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
