class Fmux < Formula
  desc "OpenAI-compatible gateway in front of every local inference server"
  homepage "https://github.com/isLinXu/fmux"

  # Builds from the git tree rather than a release tarball, so the formula is
  # usable before the first tag. Once `v0.1.0` is released, replace these two
  # lines with the tarball stanza below and fill in the checksum from
  # `shasum -a 256 fmux_0.1.0_darwin_arm64.tar.gz`:
  #
  #   url "https://github.com/isLinXu/fmux/archive/refs/tags/v0.1.0.tar.gz"
  #   sha256 "..."
  #
  # `.goreleaser.yaml` does this rewrite automatically for the tap.
  url "https://github.com/isLinXu/fmux.git", branch: "main"
  version "0.1.0"
  license "MIT"

  head "https://github.com/isLinXu/fmux.git", branch: "main"

  depends_on "go" => :build
  depends_on :macos

  def install
    ldflags = %W[
      -s -w
      -X github.com/isLinXu/fmux/internal/api.Version=#{version}
    ]
    system "go", "build", *std_go_args(ldflags: ldflags), "./cmd/fmux"
  end

  # `brew services start fmux` uses this instead of a hand-written plist.
  service do
    run [opt_bin/"fmux", "serve"]
    keep_alive crashed: true
    log_path var/"log/fmux.log"
    error_log_path var/"log/fmux.log"
    environment_variables NO_PROXY: "127.0.0.1,localhost", no_proxy: "127.0.0.1,localhost"
  end

  def caveats
    <<~EOS
      Point the fm CLI at the gateway:

        fm config add model-provider fmux http://127.0.0.1:1976/v1
        fm config set default-model-provider fmux

      Start it now, and at every login:

        brew services start fmux

      Check the wiring:

        fmux doctor
    EOS
  end

  test do
    assert_match "fmux", shell_output("#{bin}/fmux version")

    # The gateway must serve /healthz without any backend being reachable, so
    # the mock backend is the right thing to test in a bottle.
    port = free_port
    pid = fork do
      exec bin/"fmux", "serve", "--mock", "--listen", "127.0.0.1:#{port}"
    end
    begin
      Timeout.timeout(20) do
        loop do
          system "curl", "-sf", "--noproxy", "*", "http://127.0.0.1:#{port}/healthz"
          break if $CHILD_STATUS.success?
          sleep 0.2
        end
      end
      assert_match "mock-model",
                   shell_output("curl -s --noproxy * http://127.0.0.1:#{port}/v1/models")
    ensure
      Process.kill("TERM", pid)
      Process.wait(pid)
    end
  end
end
