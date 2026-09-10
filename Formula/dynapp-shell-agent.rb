class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.0"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.0/dynapp-shell-agent-0.1.0-darwin-arm64"
      sha256 "9babf0f20f929274dc0f482d4cd9d6f536c528e2f1dab1c6fe272594e33b7bf0"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.0/dynapp-shell-agent-0.1.0-darwin-amd64"
      sha256 "aa18e7395da1d0a935cb6acfc480f8edd0d6aeb100570b62db5ce1abb3b0dce8"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.0/dynapp-shell-agent-0.1.0-linux-arm64"
      sha256 "db44974626673dcc1561e03a5b9cdc0ba7882ca5d778fb4df29180e895f0de51"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.0/dynapp-shell-agent-0.1.0-linux-amd64"
      sha256 "db5d3b91f5af17d5d54390ebaa8000d30b4193d218934785e815bf42518d92ec"
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
