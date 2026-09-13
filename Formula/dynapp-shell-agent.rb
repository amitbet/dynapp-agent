class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.9"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.9/dynapp-shell-agent-0.1.9-darwin-arm64"
      sha256 "18a2115f64896d8e69df0636179c41ac5b9a3371b381c56b723453fe7deaa6e5"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.9/dynapp-shell-agent-0.1.9-darwin-amd64"
      sha256 "639071dc4cdb3a846d9285522f2cfd903e68faa957972a4ac95fb90ca019f8c5"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.9/dynapp-shell-agent-0.1.9-linux-arm64"
      sha256 "11c78114513e34c7c371977c0a5171d4565cf624e445a496a48c70082e16e217"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.9/dynapp-shell-agent-0.1.9-linux-amd64"
      sha256 "72428e7743e2a1a3ac94238859904bceea0a2bbce29e5baa2d0c36a3b19cca9a"
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
