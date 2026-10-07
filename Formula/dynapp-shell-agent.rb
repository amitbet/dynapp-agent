class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.45"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.45/dynapp-shell-agent-0.1.45-darwin-arm64"
      sha256 "c8d64346b5a587029424b3c170c1ef07e19580d02d50941103f3be11b0ef64ef"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.45/dynapp-shell-agent-0.1.45-darwin-amd64"
      sha256 "ec6ad5bac20bba1c0b421d238c5a5820d13977997bac75f4b4dbe1a0bd72e0eb"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.45/dynapp-shell-agent-0.1.45-linux-arm64"
      sha256 "9a44b40aac02efa0a6ea556c5cffaffe35d39bc2a31c1cfea3105cf326dab02a"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.45/dynapp-shell-agent-0.1.45-linux-amd64"
      sha256 "820522bab275b545624d22ec3ac63aea4dc0fa18f0c0155eccbd2a0d101987c4"
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
