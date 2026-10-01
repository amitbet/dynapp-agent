class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.37"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.37/dynapp-shell-agent-0.1.37-darwin-arm64"
      sha256 "222c9129ad27d8059ece4445e89d54727d85cb0615bc93d3cb3d4bb5c859616f"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.37/dynapp-shell-agent-0.1.37-darwin-amd64"
      sha256 "d559c4eae5af0ecd9022d9a2a8f51d5c438fbd373cc79ce5195776dda8618b16"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.37/dynapp-shell-agent-0.1.37-linux-arm64"
      sha256 "cca733f82a4c02c4ddaaf090c162e8cd541ae375f0faef9115e52ab8a5f8567d"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.37/dynapp-shell-agent-0.1.37-linux-amd64"
      sha256 "d8fd9a956347854f457e4e4d18fb3e80bc8c6c4f3098d24937270030353bee16"
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
