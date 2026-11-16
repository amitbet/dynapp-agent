class DynappShellAgent < Formula
  desc "Local agent so DynApps can use files, network, and processes"
  homepage "https://github.com/amitbet/dynapp-agent"
  version "0.1.78"
  license :cannot_represent

  livecheck do
    url :homepage
    strategy :github_latest
  end

  on_macos do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.78/dynapp-shell-agent-0.1.78-darwin-arm64"
      sha256 "5094a83b446810dc4d3b26f60b78f21e56a469b7ac2175516b83342d072e5d30"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.78/dynapp-shell-agent-0.1.78-darwin-amd64"
      sha256 "c24946f0fb972457e25351725789a9376ab75aade680aaf70613075854331902"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.78/dynapp-shell-agent-0.1.78-linux-arm64"
      sha256 "e4c4e8961b8865a5743214071d6e1f8ddfdfe8f326dfd0691d56cac76e3e954c"
    end
    on_intel do
      url "https://github.com/amitbet/dynapp-agent/releases/download/v0.1.78/dynapp-shell-agent-0.1.78-linux-amd64"
      sha256 "bed39a03c5be6b35602fed2511dbef865e1db5c603436924985ffcacb38ccaf0"
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
