class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.72"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.72/dynapp-shell-agent-0.1.72-darwin-arm64"
      sha256 "e70452803b0c4dd4d19bfdace71fbdb64674b5d9bb4c59b077b70d71ee73242f"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.72/dynapp-shell-agent-0.1.72-darwin-amd64"
      sha256 "282f3ff0f3956446949a7cf2c131671d7b5faa9f5888ab4b0552494b79681499"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.72/dynapp-shell-agent-0.1.72-linux-arm64"
      sha256 "d98d16c044b64e48e6c051712ae6306df9a1832292fbfd9eed43240db80b8f56"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.72/dynapp-shell-agent-0.1.72-linux-amd64"
      sha256 "9e1046f492b759bef6271a03f96a93cd03bd9751f837312282ce755553ac9bb4"
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
