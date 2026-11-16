class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.79"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.79/dynapp-shell-agent-0.1.79-darwin-arm64"
      sha256 "9791ceb82434195e182784cdc461b4a2aa8c86ab9885ff5eda5ea71d17298076"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.79/dynapp-shell-agent-0.1.79-darwin-amd64"
      sha256 "6bcdab381198856bcc5052ebcc2cd74691d96ddaaed0de86be3df3371c9f899e"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.79/dynapp-shell-agent-0.1.79-linux-arm64"
      sha256 "fbf55700efe4ff71576c192356346841088f351d0ceb1f824876a4150d519b6e"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.79/dynapp-shell-agent-0.1.79-linux-amd64"
      sha256 "b83f83abb22e30a963c4c8eaf987fbaedf0a2c955432913c4608441277235ab1"
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
