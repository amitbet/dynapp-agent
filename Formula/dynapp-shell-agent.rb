class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.90"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.90/dynapp-shell-agent-0.1.90-darwin-arm64"
      sha256 "9fef84edef5972329e82c1f473939a62e057dfd1b3c69a9bbe556ff926d7eb91"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.90/dynapp-shell-agent-0.1.90-darwin-amd64"
      sha256 "850851212ad9f938af23f5903906a1e931e78db44c74476f0dc734bbbc301a24"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.90/dynapp-shell-agent-0.1.90-linux-arm64"
      sha256 "95b6c3d59762db21e5b5515cea1bdfe62ac2a7778b047fd3c77b5fd07f2f7b13"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.90/dynapp-shell-agent-0.1.90-linux-amd64"
      sha256 "d290e2f561bfd476313300d88a59e0ab4cc643ed0cf5c791eeb9796e3a91d70d"
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
