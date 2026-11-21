class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.105"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.105/dynapp-shell-agent-0.1.105-darwin-arm64"
      sha256 "44e974470cbf3399efb3c6477d1f806442489af3528d1f9f0ec5da57704966cd"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.105/dynapp-shell-agent-0.1.105-darwin-amd64"
      sha256 "b92fc52c91b00f362364068eb2d0203113851898844860599d0b76460f895c46"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.105/dynapp-shell-agent-0.1.105-linux-arm64"
      sha256 "4168951240c61582135e2036d6fa2b3799f91067651173a101aa59b8abf3eef8"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.105/dynapp-shell-agent-0.1.105-linux-amd64"
      sha256 "6e32625efbcc57818d6e970ac6ec9c92ae5e7d7cece227e1fd716e92d640f99a"
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
