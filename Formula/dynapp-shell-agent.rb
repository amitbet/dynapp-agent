class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.38"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.38/dynapp-shell-agent-0.1.38-darwin-arm64"
      sha256 "f18e374f4e63f32ff17b273a333fbeb34cd4e912b249d28a01a86b3d7961ee09"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.38/dynapp-shell-agent-0.1.38-darwin-amd64"
      sha256 "8f18d8d602b3edc215ffe5e110e3cb0b45a432c3181b1f37483853044ed1ece9"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.38/dynapp-shell-agent-0.1.38-linux-arm64"
      sha256 "2932a99e7da85d297813ec0f6d40ccaa3855d099719e107503b734810a4f238e"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.38/dynapp-shell-agent-0.1.38-linux-amd64"
      sha256 "a3705e11f28d593e6f6e5937ea2883760850ef2094ce6cf307c702f8379fc32d"
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
