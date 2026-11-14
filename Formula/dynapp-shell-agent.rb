class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.70"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.70/dynapp-shell-agent-0.1.70-darwin-arm64"
      sha256 "5b4a9c82acdb831493b33b9ec9375d65ec567b7dadfe93ee61bff73a47d4647e"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.70/dynapp-shell-agent-0.1.70-darwin-amd64"
      sha256 "a59a0d210f5c5e499f6a37025b4349d6299a7e662e0095589a30649de3b89cde"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.70/dynapp-shell-agent-0.1.70-linux-arm64"
      sha256 "c1f9a5bf547803528554cac838baab05b040705d798f81b315dd33d620001e53"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.70/dynapp-shell-agent-0.1.70-linux-amd64"
      sha256 "d23492fd3c6bf6fac9f01e0406885f9e3df12bde6c91840639355bb7ec4e3066"
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
