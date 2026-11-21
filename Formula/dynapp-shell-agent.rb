class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.106"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.106/dynapp-shell-agent-0.1.106-darwin-arm64"
      sha256 "36af518eba0db1fdebe8d48f9d6e647e1db287392b6cb6036f402dd89c06dd56"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.106/dynapp-shell-agent-0.1.106-darwin-amd64"
      sha256 "4593a607e0cd95db34777e4e5746f22b05ea0057205bc1da9fbfed880281f647"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.106/dynapp-shell-agent-0.1.106-linux-arm64"
      sha256 "c62e07d39dc8134ba8e8425984c317a3ed920194bdf219950e4d360bad61f047"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.106/dynapp-shell-agent-0.1.106-linux-amd64"
      sha256 "e26fbfa74e7319de0bb5ff0c5899d4191aa533887ec08bfdd1785949eea4b335"
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
