# Legacy Homebrew formula template from the pre-cask release setup.
# GoReleaser now publishes a cask via homebrew_casks in .goreleaser.yml.

class Ax < Formula
  desc "Session switcher for LLM CLI agents"
  homepage "https://github.com/agentswitch-org/ax"
  version "GORELEASER_FILLS_THIS"
  license "MIT"

  on_macos do
    on_arm do
      url "https://github.com/agentswitch-org/ax/releases/download/vVERSION/ax_VERSION_darwin_arm64.tar.gz"
      sha256 "GORELEASER_FILLS_THIS"
    end
    on_intel do
      url "https://github.com/agentswitch-org/ax/releases/download/vVERSION/ax_VERSION_darwin_amd64.tar.gz"
      sha256 "GORELEASER_FILLS_THIS"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/agentswitch-org/ax/releases/download/vVERSION/ax_VERSION_linux_arm64.tar.gz"
      sha256 "GORELEASER_FILLS_THIS"
    end
    on_intel do
      url "https://github.com/agentswitch-org/ax/releases/download/vVERSION/ax_VERSION_linux_amd64.tar.gz"
      sha256 "GORELEASER_FILLS_THIS"
    end
  end

  def install
    bin.install "ax"
  end

  test do
    system "#{bin}/ax", "help"
  end
end
