class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.102"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.102/dynapp-shell-agent-0.1.102-darwin-arm64"
      sha256 "cf6b435d14a28534bfb315cd435b2abaa20107d2bb822ca3f3e5e82c60e6f98e"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.102/dynapp-shell-agent-0.1.102-darwin-amd64"
      sha256 "35c32b9115d9f8278b7e12440b0b815a1296caaf489340617c2c941031923bad"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.102/dynapp-shell-agent-0.1.102-linux-arm64"
      sha256 "1e238b36377d9ee5682aa3c8721f863d99b35839a3781240145af7524692d034"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.102/dynapp-shell-agent-0.1.102-linux-amd64"
      sha256 "c0e7d037aba9f55e9e4332dd5f16b91de8685ea6ddbe5767a300e0e534ead517"
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
