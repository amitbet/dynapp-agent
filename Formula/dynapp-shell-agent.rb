class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.73"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.73/dynapp-shell-agent-0.1.73-darwin-arm64"
      sha256 "263c216d24c347082461e09690a58bf58914199464a127d08b6c2a9f7025ad0b"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.73/dynapp-shell-agent-0.1.73-darwin-amd64"
      sha256 "7a307a47fb35e3194b33ce0f654a90734b8524bc9ebb07a93914e3209077f16b"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.73/dynapp-shell-agent-0.1.73-linux-arm64"
      sha256 "2c84af304daa235890bdbe9544502e17d36b5ac0286ce9c85e9dd1e375da1c56"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.73/dynapp-shell-agent-0.1.73-linux-amd64"
      sha256 "5283224a663292603910626c6b1700b0c2dd2e0ea268296e63a4fa92a45c2352"
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
