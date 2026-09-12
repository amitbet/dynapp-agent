class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.7"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.7/dynapp-shell-agent-0.1.7-darwin-arm64"
      sha256 "9c8d5f50fb85843be51cc24d3172203089553a1c86becdeeafd12fbc5ee5bd07"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.7/dynapp-shell-agent-0.1.7-darwin-amd64"
      sha256 "a64638af945bec334e136547be7cdc0312ed3498d87dbc587eca32d093a60550"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.7/dynapp-shell-agent-0.1.7-linux-arm64"
      sha256 "58f4702140fb2ac9e9fe91aa24cfd68ec049c5a645aecb98a9e585cc11df84fe"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.7/dynapp-shell-agent-0.1.7-linux-amd64"
      sha256 "240307357760a338c1f0575273624ff7cd373b34b2e3400a58c67433230d2592"
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
