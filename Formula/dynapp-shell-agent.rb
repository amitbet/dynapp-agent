class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.82"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.82/dynapp-shell-agent-0.1.82-darwin-arm64"
      sha256 "fec1f742c857a1792efbaa7e86c6fdbf8546a11f6c8424531da0bfd6787718a3"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.82/dynapp-shell-agent-0.1.82-darwin-amd64"
      sha256 "c3469868e38234c17b0d26a4586b168abbc7bf82af01cfefc58bd2b57bad6e56"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.82/dynapp-shell-agent-0.1.82-linux-arm64"
      sha256 "503d5d5c3d4ffa82e7b250c26108ad6d280aed0d336562eeabe3dfbb07954942"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.82/dynapp-shell-agent-0.1.82-linux-amd64"
      sha256 "4ec93d668011c66c0311dc9bbcd2b0adc7ad397e5b1ae87265633c0adc7b3062"
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
