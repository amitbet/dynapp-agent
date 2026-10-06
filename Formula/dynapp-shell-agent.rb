class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.44"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.44/dynapp-shell-agent-0.1.44-darwin-arm64"
      sha256 "ec66bcc2011069e6eec4e86dc5f3cbf16684646baf2bc353677274927b0544f0"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.44/dynapp-shell-agent-0.1.44-darwin-amd64"
      sha256 "f247dcb1cb2787bee32d18fc4ae0fcbe61e017c6f1f7941825d6a0d17a535d55"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.44/dynapp-shell-agent-0.1.44-linux-arm64"
      sha256 "4460cae32844e96ad12ea8cc53715eb42967039d2b335a06ce845d730b35b795"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.44/dynapp-shell-agent-0.1.44-linux-amd64"
      sha256 "5dd56ad53ebbb4ffb83a5ca397715bf65032196fe027ba1c6c46768f90a2ea0f"
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
