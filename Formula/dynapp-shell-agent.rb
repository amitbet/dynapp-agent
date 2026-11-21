class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.104"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.104/dynapp-shell-agent-0.1.104-darwin-arm64"
      sha256 "4150919fa0135b5293af2e0e834abe83bbd6a11196a0533e8831c6f7c2d56eac"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.104/dynapp-shell-agent-0.1.104-darwin-amd64"
      sha256 "66df919496456041672e5dee6e8f8bb727181c2984ac55915229fbc305f0b428"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.104/dynapp-shell-agent-0.1.104-linux-arm64"
      sha256 "27c5508e2677633203b8f84d6abd77f060d6c29aa98380631c7e99f9b2889e82"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.104/dynapp-shell-agent-0.1.104-linux-amd64"
      sha256 "eae680d7a918fc9a9514bc92fbff89cb2b66b56fef657532f83d69744904e7d1"
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
