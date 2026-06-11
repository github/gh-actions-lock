# frozen_string_literal: true

# Integration test harness for gh-actions-pin.
#
# Provides a Scenario DSL that:
#   - Creates a temp directory with .github/workflows/ and optional lockfile
#   - Optionally starts a stub HTTP server to mock GitHub API responses
#   - Runs the compiled binary and captures stdout, stderr, exit code
#   - Asserts on output patterns, exit codes, and file mutations
#
# Usage:
#   ruby test/integration/run.rb            # run all scenarios
#   ruby test/integration/run.rb happy_path # run one by name

require "fileutils"
require "json"
require "open3"
require "openssl"
require "tmpdir"
require "webrick"
require "webrick/https"
require "yaml"
require "logger"

module ActionsPin
  module Integration
    # ── Result ──────────────────────────────────────────────────────────
    Result = Struct.new(:stdout, :stderr, :status, :dir, keyword_init: true) do
      def exit_code; status.exitstatus; end
      def success?;  status.success?;   end
      def output;    stderr;            end # gh extensions write UI to stderr
    end

    # ── StubServer ──────────────────────────────────────────────────────
    # Lightweight WEBrick server that replays canned API responses.
    # Routes are registered as [method, path_pattern] → response proc.
    class StubServer
      attr_reader :port, :url

      def initialize
        @routes = []
        @port = nil
        @server = nil
        @url = nil
        @fallback_status = 404
        @fallback_body = '{"message":"Not Found"}'
        @fallback_headers = {}
      end

      # Register a route. Pattern can be a String (exact) or Regexp.
      # Block receives (request) and returns [status, headers, body].
      def on(method, pattern, &block)
        @routes << { method: method.to_s.upcase, pattern: pattern, handler: block }
        self
      end

      # Convenience: register a GET that returns 200 JSON.
      def get(pattern, body, headers: {})
        on(:GET, pattern) { [200, { "Content-Type" => "application/json" }.merge(headers), body.is_a?(String) ? body : JSON.generate(body)] }
      end

      # Convenience: register a GET that returns 403 with optional headers.
      def get_forbidden(pattern, body, headers: {})
        on(:GET, pattern) { [403, { "Content-Type" => "application/json" }.merge(headers), body.is_a?(String) ? body : JSON.generate(body)] }
      end

      def start
        # Generate a self-signed cert for HTTPS
        key = OpenSSL::PKey::RSA.new(2048)
        cert = OpenSSL::X509::Certificate.new
        cert.version = 2
        cert.serial = 1
        cert.subject = OpenSSL::X509::Name.parse("/CN=localhost")
        cert.issuer = cert.subject
        cert.public_key = key.public_key
        cert.not_before = Time.now - 3600
        cert.not_after = Time.now + 3600
        # Add SAN for IP-based access
        ef = OpenSSL::X509::ExtensionFactory.new
        ef.subject_certificate = cert
        ef.issuer_certificate = cert
        cert.add_extension(ef.create_extension("subjectAltName", "IP:127.0.0.1,DNS:localhost"))
        cert.sign(key, OpenSSL::Digest::SHA256.new)

        @server = WEBrick::HTTPServer.new(
          Port: 0,
          Logger: WEBrick::Log.new("/dev/null"),
          AccessLog: [],
          SSLEnable: true,
          SSLCertificate: cert,
          SSLPrivateKey: key,
          SSLVerifyClient: OpenSSL::SSL::VERIFY_NONE,
        )
        @port = @server.config[:Port]
        @url = "https://127.0.0.1:#{@port}"

        @server.mount_proc "/" do |req, res|
          matched = @routes.find do |r|
            r[:method] == req.request_method &&
              case r[:pattern]
              when Regexp then req.path.match?(r[:pattern])
              when String then req.path == r[:pattern]
              end
          end

          if matched
            status, headers, body = matched[:handler].call(req)
            res.status = status
            headers.each { |k, v| res[k] = v }
            res.body = body
          else
            res.status = @fallback_status
            @fallback_headers.each { |k, v| res[k] = v }
            res.body = @fallback_body
          end
        end

        @thread = Thread.new { @server.start }
        # Wait for server to be ready
        sleep 0.1 until @server.status == :Running
        self
      end

      def stop
        @server&.shutdown
        @thread&.join(5)
      end
    end

    # ── Scenario ────────────────────────────────────────────────────────
    class Scenario
      attr_reader :name, :failures

      def initialize(name)
        @name = name
        @workflows = {}
        @lockfile = nil
        @stub_server = nil
        @env = {}
        @args = []
        @assertions = []
        @failures = []
        @setup_blocks = []
      end

      # ── DSL: fixtures ────────────────────────────────────────────────

      def workflow(path, content)
        @workflows[path] = content
        self
      end

      def lockfile(content)
        @lockfile = content
        self
      end

      def env(hash)
        @env.merge!(hash)
        self
      end

      def args(*a)
        @args = a.flatten
        self
      end

      def stub_server(&block)
        @stub_server = StubServer.new
        block.call(@stub_server) if block
        self
      end

      def setup(&block)
        @setup_blocks << block
        self
      end

      # ── DSL: assertions ──────────────────────────────────────────────

      def assert_exit(code)
        @assertions << -> (r) { assert_eq("exit code", r.exit_code, code) }
        self
      end

      def assert_success
        assert_exit(0)
      end

      def assert_failure
        @assertions << -> (r) { assert_true("non-zero exit", !r.success?) }
        self
      end

      def assert_output_contains(*patterns)
        patterns.each do |pat|
          @assertions << -> (r) { assert_match("output contains #{pat.inspect}", r.output, pat) }
        end
        self
      end

      def assert_output_excludes(*patterns)
        patterns.each do |pat|
          @assertions << -> (r) { assert_no_match("output excludes #{pat.inspect}", r.output, pat) }
        end
        self
      end

      def assert_stdout_contains(*patterns)
        patterns.each do |pat|
          @assertions << -> (r) { assert_match("stdout contains #{pat.inspect}", r.stdout, pat) }
        end
        self
      end

      def assert_lockfile_exists
        @assertions << -> (r) { assert_true("lockfile exists", File.exist?(File.join(r.dir, ".github", "workflows", "actions.lock"))) }
        self
      end

      def assert_lockfile_contains(*patterns)
        patterns.each do |pat|
          @assertions << -> (r) {
            lockpath = File.join(r.dir, ".github", "workflows", "actions.lock")
            content = File.read(lockpath) rescue ""
            assert_match("lockfile contains #{pat.inspect}", content, pat)
          }
        end
        self
      end

      def assert_custom(&block)
        @assertions << block
        self
      end

      # ── Execution ───────────────────────────────────────────────────

      def run(binary)
        dir = Dir.mktmpdir("actions-pin-test-")
        server = nil

        begin
          # Write workflow files
          @workflows.each do |path, content|
            full = File.join(dir, ".github", "workflows", path)
            FileUtils.mkdir_p(File.dirname(full))
            File.write(full, content)
          end

          # Write lockfile
          if @lockfile
            lock_path = File.join(dir, ".github", "workflows", "actions.lock")
            FileUtils.mkdir_p(File.dirname(lock_path))
            File.write(lock_path, @lockfile)
          end

          # Initialize git repo (required for gh repo detection)
          Dir.chdir(dir) do
            system("git init -q .", exception: true)
            system("git add -A", exception: true)
            system("git", "-c", "user.email=test@test", "-c", "user.name=Test", "commit", "-q", "-m", "init", "--allow-empty", exception: true)
          end

          # Start stub server
          if @stub_server
            server = @stub_server
            server.start
            # go-gh treats non-github.com hosts as enterprise:
            #   REST  → https://<host>/api/v3/
            #   GQL   → https://<host>/api/graphql
            # We include the port in the hostname so go-gh routes to our server.
            @env["GH_HOST"] = "127.0.0.1:#{server.port}"
            # go-gh uses GH_ENTERPRISE_TOKEN for non-github.com hosts
            token = @env.delete("GH_TOKEN") || "stub-token"
            @env["GH_ENTERPRISE_TOKEN"] = token
            # Skip TLS verification for the self-signed stub cert
            @env["GH_ACTIONS_PIN_INSECURE"] = "1"
          end

          # Run setup blocks
          @setup_blocks.each { |b| b.call(dir) }

          # Build command
          run_env = @env.dup
          # Use a cache dir inside the temp dir to avoid polluting real cache
          run_env["GH_ACTIONS_PIN_CACHE_DIR"] = File.join(dir, ".cache")

          cmd = [binary] + @args
          stdout, stderr, status = Open3.capture3(run_env, *cmd, chdir: dir)
          result = Result.new(stdout: stdout, stderr: stderr, status: status, dir: dir)

          # Run assertions
          @assertions.each { |a| a.call(result) }

          result
        ensure
          server&.stop
          FileUtils.rm_rf(dir) unless ENV["KEEP_FIXTURES"]
        end
      end

      private

      def assert_eq(label, got, want)
        return if got == want
        @failures << "#{label}: got #{got.inspect}, want #{want.inspect}"
      end

      def assert_true(label, value)
        return if value
        @failures << "#{label}: expected truthy"
      end

      def assert_match(label, text, pattern)
        matched = case pattern
                  when Regexp then text.match?(pattern)
                  when String then text.include?(pattern)
                  end
        return if matched
        @failures << "#{label}\n  in text:\n#{indent(text)}"
      end

      def assert_no_match(label, text, pattern)
        matched = case pattern
                  when Regexp then text.match?(pattern)
                  when String then text.include?(pattern)
                  end
        return unless matched
        @failures << "#{label}\n  unexpectedly found in:\n#{indent(text)}"
      end

      def indent(text)
        text.lines.first(20).map { |l| "    #{l}" }.join
      end
    end

    # ── Runner ──────────────────────────────────────────────────────────
    class Runner
      attr_reader :scenarios

      def initialize(binary: nil)
        @binary = binary || find_binary
        @scenarios = []
      end

      def scenario(name, &block)
        s = Scenario.new(name)
        block.call(s)
        @scenarios << s
        s
      end

      def run(filter: nil)
        to_run = filter ? @scenarios.select { |s| s.name.to_s.include?(filter) } : @scenarios
        puts "Running #{to_run.size} integration scenario(s)...\n\n"

        passed = 0
        failed = 0

        to_run.each do |s|
          print "  #{s.name} ... "
          begin
            s.run(@binary)
            if s.failures.empty?
              puts "\e[32m✓\e[0m"
              passed += 1
            else
              puts "\e[31m✗\e[0m"
              s.failures.each { |f| puts "    \e[31m#{f}\e[0m" }
              failed += 1
            end
          rescue => e
            puts "\e[31m✗ (exception)\e[0m"
            puts "    #{e.class}: #{e.message}"
            puts e.backtrace.first(5).map { |l| "    #{l}" }.join("\n")
            failed += 1
          end
        end

        puts "\n#{passed} passed, #{failed} failed"
        exit(failed > 0 ? 1 : 0)
      end

      private

      def find_binary
        # Try the built binary in the repo, then PATH
        repo_bin = File.expand_path("../../../gh-actions-pin", __FILE__)
        return repo_bin if File.executable?(repo_bin)

        gobin = File.join(ENV["HOME"], "go", "bin", "gh-actions-pin")
        return gobin if File.executable?(gobin)

        raise "Cannot find gh-actions-pin binary. Run `make build` first."
      end
    end
  end
end
