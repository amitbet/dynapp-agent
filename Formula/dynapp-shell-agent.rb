class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.25"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.25/dynapp-shell-agent-0.1.25-darwin-arm64"
      sha256 "4a652f02d2905c917c0d5093cb9fb4f7c852b3b013cb6a23e88f2c74bb4adfd8"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.25/dynapp-shell-agent-0.1.25-darwin-amd64"
      sha256 "66ab3db471d8c942351d0c3fab2972f37a6b8acee13fed944da0acdcf69d5444"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.25/dynapp-shell-agent-0.1.25-linux-arm64"
      sha256 "63528448b179e063f51f311fd419f3ede98a0735aeae33f6ed1fd97011da71cf"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.25/dynapp-shell-agent-0.1.25-linux-amd64"
      sha256 "c4e639f672fe231ed304a5380874de501ea7bc9e0c7ca115b8213c5c0adf9063"
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
