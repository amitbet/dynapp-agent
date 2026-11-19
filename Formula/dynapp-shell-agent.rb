class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.87"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.87/dynapp-shell-agent-0.1.87-darwin-arm64"
      sha256 "6b6a959512d51bdd804775af2950c25b21d0594042b8d907987d17a7349c316f"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.87/dynapp-shell-agent-0.1.87-darwin-amd64"
      sha256 "7eb7b0898789d6a52df7318ceb80aa73a9ed751e31e3aa98f17063680b95d5bc"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.87/dynapp-shell-agent-0.1.87-linux-arm64"
      sha256 "ff7be8c370a956fdebf02e51f01868e21f7e3c350af3b5d9d5a7e5df2958f7d7"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.87/dynapp-shell-agent-0.1.87-linux-amd64"
      sha256 "bdf51aa2ef4082c19cdfefbdb99306fb31147bcd4a28ad869d941b7b537c12d7"
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
