class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.30"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.30/dynapp-shell-agent-0.1.30-darwin-arm64"
      sha256 "91f49ca392bc96d9c66e3768e808205c3f193c38f6396ce9453d54002a893532"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.30/dynapp-shell-agent-0.1.30-darwin-amd64"
      sha256 "a764db866f66844bb4768d6286f7fd89cf7fed12246e1cac3c1ac8d8683d4336"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.30/dynapp-shell-agent-0.1.30-linux-arm64"
      sha256 "946f654839d18cea21be4e61c43e245f3e4259bbc6a89c5343ba941ba415b1f3"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.30/dynapp-shell-agent-0.1.30-linux-amd64"
      sha256 "6c3f5e2fb888a33042d27f80ce5c2a031bd23c13acbca5efe4e09695a1bfbba8"
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
