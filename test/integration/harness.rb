# frozen_string_literal: true

# Integration test harness for gh-actions-pin.
#
# Modes:
#   Batch:       ruby test/integration/run.rb [filter]
#   Interactive: ruby test/integration/run.rb --shell
#
# In shell mode every scenario runs through a PTY so you see real ANSI
# colors, icons, and spinner output exactly as a user would.

require "fileutils"
require "io/console"
require "json"
require "open3"
require "openssl"
require "pty"
require "shellwords"
require "tmpdir"
require "webrick"
require "webrick/https"
require "yaml"

module ActionsPin
  module Integration
    # ── Result ──────────────────────────────────────────────────────────
    Result = Struct.new(:stdout, :stderr, :status, :dir, keyword_init: true) do
      def exit_code; status.exitstatus; end
      def success?;  status.success?;   end
      def output;    stderr;            end # gh extensions write UI to stderr
    end

    # ── PTY Result ──────────────────────────────────────────────────────
    # When run through a PTY, stdout and stderr are merged into one stream.
    PTYResult = Struct.new(:combined, :exit_code, :dir, keyword_init: true) do
      def success?; exit_code == 0; end
      def output;   combined;       end
      def stdout;   combined;       end
      def stderr;   combined;       end
      def status;   self;           end
      def exitstatus; exit_code;    end
    end

    # ── StubServer ──────────────────────────────────────────────────────
    class StubServer
      attr_reader :port, :url

      def initialize
        @routes = []
        @port = nil
        @server = nil
        @url = nil
      end

      def on(method, pattern, &block)
        @routes << { method: method.to_s.upcase, pattern: pattern, handler: block }
        self
      end

      def get(pattern, body, headers: {})
        on(:GET, pattern) { [200, { "Content-Type" => "application/json" }.merge(headers), body.is_a?(String) ? body : JSON.generate(body)] }
      end

      def get_forbidden(pattern, body, headers: {})
        on(:GET, pattern) { [403, { "Content-Type" => "application/json" }.merge(headers), body.is_a?(String) ? body : JSON.generate(body)] }
      end

      def start
        key = OpenSSL::PKey::RSA.new(2048)
        cert = OpenSSL::X509::Certificate.new
        cert.version = 2
        cert.serial = 1
        cert.subject = OpenSSL::X509::Name.parse("/CN=localhost")
        cert.issuer = cert.subject
        cert.public_key = key.public_key
        cert.not_before = Time.now - 3600
        cert.not_after = Time.now + 3600
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
            res.status = 404
            res.body = '{"message":"Not Found"}'
          end
        end

        @thread = Thread.new { @server.start }
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

      # ── DSL: fixtures ──────────────────────────────────────────────

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

      # ── DSL: assertions ────────────────────────────────────────────

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

      # ── Execution ──────────────────────────────────────────────────

      # Prepare fixtures without running. Returns a Context.
      def prepare(binary)
        dir = Dir.mktmpdir("actions-pin-test-")

        @workflows.each do |path, content|
          full = File.join(dir, ".github", "workflows", path)
          FileUtils.mkdir_p(File.dirname(full))
          File.write(full, content)
        end

        if @lockfile
          lock_path = File.join(dir, ".github", "workflows", "actions.lock")
          FileUtils.mkdir_p(File.dirname(lock_path))
          File.write(lock_path, @lockfile)
        end

        Dir.chdir(dir) do
          system("git init -q .", exception: true)
          system("git add -A", exception: true)
          system("git", "-c", "user.email=test@test", "-c", "user.name=Test",
                 "commit", "-q", "-m", "init", "--allow-empty", exception: true)
        end

        server = nil
        env = @env.dup
        if @stub_server
          server = @stub_server
          server.start
          env["GH_HOST"] = "127.0.0.1:#{server.port}"
          token = env.delete("GH_TOKEN") || "stub-token"
          env["GH_ENTERPRISE_TOKEN"] = token
          env["GH_ACTIONS_PIN_INSECURE"] = "1"
        end

        @setup_blocks.each { |b| b.call(dir) }
        env["GH_ACTIONS_PIN_CACHE_DIR"] = File.join(dir, ".cache")

        Context.new(dir: dir, env: env, cmd: [binary] + @args,
                    server: server, scenario: self)
      end

      # Batch mode: capture output, run assertions.
      def run(binary)
        ctx = prepare(binary)
        begin
          result = ctx.run_captured
          @assertions.each { |a| a.call(result) }
          result
        ensure
          ctx.teardown unless ENV["KEEP_FIXTURES"]
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

    # ── Context ─────────────────────────────────────────────────────────
    # A prepared scenario environment. Supports captured runs (batch) and
    # PTY runs (interactive shell) so you see real ANSI output.
    class Context
      attr_reader :dir, :env, :cmd, :server, :scenario

      def initialize(dir:, env:, cmd:, server:, scenario:)
        @dir = dir
        @env = env
        @cmd = cmd
        @server = server
        @scenario = scenario
        @torn_down = false
      end

      # Capture stdout/stderr separately (for assertions). No TTY.
      def run_captured
        stdout, stderr, status = Open3.capture3(@env, *@cmd, chdir: @dir)
        Result.new(stdout: stdout, stderr: stderr, status: status, dir: @dir)
      end

      # Run through a PTY so the binary sees a real terminal and emits
      # ANSI colors, icons, and spinners. Output streams live to $stdout.
      # Returns a PTYResult with the merged output.
      def run_pty
        flat_env = @env.map { |k, v| "#{k}=#{Shellwords.shellescape(v)}" }
        shell_cmd = "cd #{Shellwords.shellescape(@dir)} && " +
                    flat_env.join(" ") + " " +
                    @cmd.map { |c| Shellwords.shellescape(c) }.join(" ")

        combined = String.new
        exit_code = nil

        begin
          PTY.spawn("/bin/bash", "-c", shell_cmd) do |reader, _writer, pid|
            begin
              reader.each_char do |ch|
                $stdout.print ch       # live to terminal
                combined << ch
              end
            rescue Errno::EIO
              # PTY closed — normal on macOS when process exits
            end
            _, status = Process.wait2(pid)
            exit_code = status.exitstatus
          end
        rescue PTY::ChildExited => e
          exit_code = e.status.exitstatus
        end

        PTYResult.new(combined: combined, exit_code: exit_code || 1, dir: @dir)
      end

      def env_exports
        @env.map { |k, v| "export #{k}=#{Shellwords.shellescape(v)}" }.join("\n")
      end

      def cmd_string
        @cmd.map { |c| Shellwords.shellescape(c) }.join(" ")
      end

      def teardown
        return if @torn_down
        @torn_down = true
        @server&.stop
        FileUtils.rm_rf(@dir)
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

      # ── Batch mode ─────────────────────────────────────────────────

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

      # ── Interactive shell ──────────────────────────────────────────

      def shell
        puts "\e[1mgh-actions-pin integration shell\e[0m"
        puts "Binary: #{@binary}"
        puts "Scenarios: #{@scenarios.map(&:name).join(', ')}"
        puts
        puts "Commands:"
        puts "  \e[36mlist\e[0m                  Show all scenarios"
        puts "  \e[36mrun <name>\e[0m            Run scenario with live PTY output"
        puts "  \e[36mrun all\e[0m               Run all scenarios with live output"
        puts "  \e[36mtest [filter]\e[0m         Batch-test scenarios (captured, assertions)"
        puts "  \e[36minspect <name>\e[0m        Show scenario fixtures without running"
        puts "  \e[36mcd <name>\e[0m             Prepare scenario and drop into its dir"
        puts "  \e[36mquit\e[0m                  Exit"
        puts

        active_ctx = nil

        loop do
          prompt = active_ctx ? "\e[33m#{active_ctx.scenario.name}\e[0m > " : "\e[35mpin-test\e[0m > "
          print prompt
          line = $stdin.gets
          break if line.nil?
          line = line.strip
          next if line.empty?

          parts = line.split(/\s+/, 2)
          verb = parts[0]
          arg  = parts[1]

          case verb
          when "quit", "exit", "q"
            active_ctx&.teardown
            break

          when "list", "ls"
            @scenarios.each_with_index do |s, i|
              puts "  \e[36m#{s.name}\e[0m"
            end

          when "run"
            active_ctx&.teardown
            active_ctx = nil

            if arg == "all"
              run_all_live
            else
              s = find_scenario(arg)
              next unless s
              run_one_live(s)
            end

          when "test"
            to_run = arg ? @scenarios.select { |s| s.name.to_s.include?(arg) } : @scenarios
            run_batch(to_run)

          when "inspect"
            s = find_scenario(arg)
            next unless s
            ctx = s.prepare(@binary)
            puts "\e[1mScenario:\e[0m #{s.name}"
            puts "\e[1mDir:\e[0m      #{ctx.dir}"
            puts "\e[1mCmd:\e[0m      #{ctx.cmd_string}"
            if ctx.server
              puts "\e[1mStub:\e[0m     #{ctx.server.url}"
            end
            puts "\e[1mWorkflows:\e[0m"
            Dir.glob("#{ctx.dir}/.github/workflows/*").each do |f|
              puts "  #{File.basename(f)}"
            end
            puts
            puts "Run \e[36mrun #{s.name}\e[0m to execute, or \e[36mcd #{s.name}\e[0m to explore."
            # Don't teardown — keep it alive for follow-up commands
            active_ctx&.teardown
            active_ctx = ctx

          when "cd"
            active_ctx&.teardown
            s = find_scenario(arg)
            next unless s
            ctx = s.prepare(@binary)
            active_ctx = ctx
            puts "\e[1mPrepared:\e[0m #{s.name}"
            puts "\e[1mDir:\e[0m      #{ctx.dir}"
            if ctx.server
              puts "\e[1mStub:\e[0m     #{ctx.server.url}"
            end
            puts
            puts "Dropping into subshell. The env is set up — run the binary with:"
            puts "  \e[36m#{ctx.cmd_string}\e[0m"
            puts "Type \e[36mexit\e[0m to return to the integration shell."
            puts

            # Spawn a real subshell with the scenario env
            sub_env = ctx.env.merge("PS1" => "\e[33m#{s.name}\e[0m \\$ ")
            system(sub_env, ENV.fetch("SHELL", "/bin/bash"), chdir: ctx.dir)
            puts "\nBack in integration shell. Scenario dir still live at #{ctx.dir}"

          when "rerun"
            if active_ctx
              puts "\e[1m── re-running #{active_ctx.scenario.name} ──\e[0m\n\n"
              active_ctx.run_pty
              puts
            else
              puts "No active scenario. Use \e[36mrun <name>\e[0m first."
            end

          else
            puts "Unknown command: #{verb}. Type \e[36mlist\e[0m for scenarios."
          end
        end

        active_ctx&.teardown
        puts "Bye."
      end

      private

      def find_binary
        repo_bin = File.expand_path("../../../gh-actions-pin", __FILE__)
        return repo_bin if File.executable?(repo_bin)

        gobin = File.join(ENV["HOME"], "go", "bin", "gh-actions-pin")
        return gobin if File.executable?(gobin)

        raise "Cannot find gh-actions-pin binary. Run `make build` first."
      end

      def find_scenario(name)
        unless name
          puts "Usage: run <scenario-name>"
          return nil
        end
        matches = @scenarios.select { |s| s.name.to_s.include?(name) }
        if matches.empty?
          puts "No scenario matching '#{name}'. Try \e[36mlist\e[0m."
          return nil
        end
        if matches.size > 1
          puts "Ambiguous: #{matches.map(&:name).join(', ')}"
          return nil
        end
        matches.first
      end

      def run_one_live(s)
        puts "\e[1m── #{s.name} ──\e[0m\n\n"
        ctx = s.prepare(@binary)
        begin
          result = ctx.run_pty
          puts
          if result.success?
            puts "  \e[32m✓ exit 0\e[0m"
          else
            puts "  \e[31m✗ exit #{result.exit_code}\e[0m"
          end
          puts
        ensure
          ctx.teardown unless ENV["KEEP_FIXTURES"]
        end
      end

      def run_all_live
        @scenarios.each { |s| run_one_live(s) }
      end

      def run_batch(to_run)
        puts "Running #{to_run.size} scenario(s)...\n\n"
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
      end
    end
  end
end
