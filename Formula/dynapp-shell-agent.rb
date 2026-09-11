class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.2"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.2/dynapp-shell-agent-0.1.2-darwin-arm64"
      sha256 "5ca6610099e67cf1df98899099f1f810b1906d55d1e65d5b3c4411924860ce49"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.2/dynapp-shell-agent-0.1.2-darwin-amd64"
      sha256 "2fe2d0d351a43b0f28ebcba48e437722c66da7d77f40cc17143e5225c987db7a"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.2/dynapp-shell-agent-0.1.2-linux-arm64"
      sha256 "757398c5361841ab427828cf303ef583a58d17a7123e663100892ecca39299c8"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.2/dynapp-shell-agent-0.1.2-linux-amd64"
      sha256 "7cbacab5aa28fc617e47bb1a0a0f7c6bf3826bdbb28e2cabf16285e63f73dc2e"
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
