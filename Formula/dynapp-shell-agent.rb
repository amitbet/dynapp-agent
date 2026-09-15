class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.16"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.16/dynapp-shell-agent-0.1.16-darwin-arm64"
      sha256 "94eb33d0ec589c319fc96c1dca1fa5c42b8039c39737e3c1607f79669524da01"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.16/dynapp-shell-agent-0.1.16-darwin-amd64"
      sha256 "ddbde8837c7cce08dbcaef7abb2bd49da12e6adf667614e5698d2796e6bdd2ff"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.16/dynapp-shell-agent-0.1.16-linux-arm64"
      sha256 "62e6c5922ad2e0fb6a52c64022f34981f52e7ca7f59749c841a3057012083408"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.16/dynapp-shell-agent-0.1.16-linux-amd64"
      sha256 "9b7a507e5da2be80f3760954792afc7b120c519284183489e97c95b43a370b1f"
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
