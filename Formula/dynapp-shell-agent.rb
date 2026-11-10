class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.57"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.57/dynapp-shell-agent-0.1.57-darwin-arm64"
      sha256 "ac2d8949d24d9d8722d79f973e4740ecf19a1eaeb196cfba71300278deb93ed7"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.57/dynapp-shell-agent-0.1.57-darwin-amd64"
      sha256 "710931cca3abe685b3c88371d87f535cd448f926dab115baae12f2cff4ac3256"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.57/dynapp-shell-agent-0.1.57-linux-arm64"
      sha256 "b34d2c4acb0dc112563f8ec867aef98301d5b908ae2dd93aa35785975d1adb4e"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.57/dynapp-shell-agent-0.1.57-linux-amd64"
      sha256 "0d79819c51d43bf9397c6cbca5cad2198e959f57c4ba2bb05cf54b57553ce9ff"
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
