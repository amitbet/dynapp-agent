class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.81"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.81/dynapp-shell-agent-0.1.81-darwin-arm64"
      sha256 "695549a0d64d6cb4a337d9e5192d3496ead1926829762344256fe75f8c9652be"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.81/dynapp-shell-agent-0.1.81-darwin-amd64"
      sha256 "75d30426e8acbbc0420cc61699cc1c4402148faf9634e00a0d17db2b68fa3ff3"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.81/dynapp-shell-agent-0.1.81-linux-arm64"
      sha256 "b18d22f7f14329983ded6fe60830c803d515e4ed2129fab18d4e26961d21ac4f"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.81/dynapp-shell-agent-0.1.81-linux-amd64"
      sha256 "320b538f7a32327172b5b368bffc74bc6142f3c36689615fe907dea2dcbd8953"
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
