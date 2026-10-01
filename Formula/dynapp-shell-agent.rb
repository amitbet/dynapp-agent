class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.34"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.34/dynapp-shell-agent-0.1.34-darwin-arm64"
      sha256 "e2c534df19a0ca6914fedb32afbdee0d5ffdeccfb3c52552a0df65f9e3064b39"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.34/dynapp-shell-agent-0.1.34-darwin-amd64"
      sha256 "a960224e29aaaa263de27a4d062ffd0bd9d1e64553188aa9c6a5491dc541154b"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.34/dynapp-shell-agent-0.1.34-linux-arm64"
      sha256 "e919f8cddeb1de4c08bac9c907bcad158d4e049ed84cf1ec508e465e60a82046"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.34/dynapp-shell-agent-0.1.34-linux-amd64"
      sha256 "2fd5647070759c87c51ad8f433e7e2298c38427160efa3690568dbca3f4b111d"
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
