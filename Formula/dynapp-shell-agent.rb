class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.23"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.23/dynapp-shell-agent-0.1.23-darwin-arm64"
      sha256 "3aad8b6fd793623d16d044ce0342289be05b5afebc61013d2d22f733d59b71cf"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.23/dynapp-shell-agent-0.1.23-darwin-amd64"
      sha256 "da4437a8fa277d1fc73a9c5c8f91a7330d0741ceea1370b8e1f8bce22aa30905"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.23/dynapp-shell-agent-0.1.23-linux-arm64"
      sha256 "60dfdcd66e392e132411c45e0cc542d9c21a5f32e9b04bcd69d0c79d4ffc4b89"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.23/dynapp-shell-agent-0.1.23-linux-amd64"
      sha256 "28699a8166e59c5357169f6603ce51d5c2c2458874cc0263a57e4be28be4c00f"
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
