class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.13"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.13/dynapp-shell-agent-0.1.13-darwin-arm64"
      sha256 "a1267d5ce60acecd5adb6aa69017462201c8058e85ec5657f777c916fa75e80e"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.13/dynapp-shell-agent-0.1.13-darwin-amd64"
      sha256 "e5214c020136a6cd7e9b0c12ac4afbc02d41d5eb271e650e8f4def1026d60e96"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.13/dynapp-shell-agent-0.1.13-linux-arm64"
      sha256 "f6db6fada5f3e6c349e7d82d68ce31913ac4f624dbffc60a4d641d40d77e4903"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.13/dynapp-shell-agent-0.1.13-linux-amd64"
      sha256 "ceb9ea26c09858ae22d4f2e8848d1469abbe3d0a14292c55498af4b9372ea3e6"
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
