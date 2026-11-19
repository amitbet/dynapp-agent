class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.89"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.89/dynapp-shell-agent-0.1.89-darwin-arm64"
      sha256 "e69d362d5e19e06c81d3ebe75666430696c37cf375c7171e7af927b6243b67a4"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.89/dynapp-shell-agent-0.1.89-darwin-amd64"
      sha256 "0ed1adfc723cc73956b58159f82b3119c388a8c125f8c35eee8c9c808a2f4466"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.89/dynapp-shell-agent-0.1.89-linux-arm64"
      sha256 "6b5e27099578f633808b5d00663be731f47fbabe3737eca30a6692f7197fed43"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.89/dynapp-shell-agent-0.1.89-linux-amd64"
      sha256 "39b4d81d16a1241f4fc69f60ae6cbfd1b3420dfd1b7faa066c460087f9c0c54a"
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
