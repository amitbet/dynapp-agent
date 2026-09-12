class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.4"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.4/dynapp-shell-agent-0.1.4-darwin-arm64"
      sha256 "fbd533607480f6ab018326525a92955aa8364ccd92a2e8199bb4c36017d52028"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.4/dynapp-shell-agent-0.1.4-darwin-amd64"
      sha256 "feb54f9e7cbbed87ea2f7633646fccd65fd863c547dd98eef1dfb854990b3dfa"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.4/dynapp-shell-agent-0.1.4-linux-arm64"
      sha256 "9ddb54ea13ab3b26764fa21c663d8066c329612caad1bb2d4b2a0f4c4df7852b"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.4/dynapp-shell-agent-0.1.4-linux-amd64"
      sha256 "353d08c2b5838b50f1b8c68076a04c6ab8a647a53ebe27adedecfaf543cb5d55"
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
