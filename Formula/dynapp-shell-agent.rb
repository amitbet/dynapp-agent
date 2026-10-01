class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.36"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.36/dynapp-shell-agent-0.1.36-darwin-arm64"
      sha256 "ebdb0cad4007bae930fb5f204ea30d9666628726bacc2e918827da1d158b274a"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.36/dynapp-shell-agent-0.1.36-darwin-amd64"
      sha256 "51f192afe6594d6a6ccea1871047aaa517ccd6a62e6beae73ba69730ee65d493"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.36/dynapp-shell-agent-0.1.36-linux-arm64"
      sha256 "007972a86a72535134528350eb7c9c34de28f6ed57fcd11eca5adb00d4d22e01"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.36/dynapp-shell-agent-0.1.36-linux-amd64"
      sha256 "e5504bd75521ad280eaf332ab5110ff25bbf8492be6e528b3264f03620d7817f"
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
