class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.48"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.48/dynapp-shell-agent-0.1.48-darwin-arm64"
      sha256 "8963468e44faf5f7755665162266f232c6ed9bfdb855686885841f18a166e8ef"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.48/dynapp-shell-agent-0.1.48-darwin-amd64"
      sha256 "45e910c32d2974fafbdd24461fa440c2cd3c83f99a9cc7e920e39fcae77eaacb"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.48/dynapp-shell-agent-0.1.48-linux-arm64"
      sha256 "e87caae22830d50a2e4f6088099059fe76f13ee92147799617b77713dd5690b0"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.48/dynapp-shell-agent-0.1.48-linux-amd64"
      sha256 "2a45dd1211d73d3c2fd33701592514e400ce72af7a92db23a9f34e4a42b94fd1"
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
