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
require "reline"
require "set"
require "shellwords"
require "tmpdir"
require "webrick"
require "webrick/https"
require "yaml"

module ActionsPin
  module Integration
    # Raised to skip a scenario gracefully (e.g. missing token for live tests).
    class SkipScenario < StandardError; end

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

    # ── Catalog ─────────────────────────────────────────────────────────
    # Loads the shared scenario catalog from test/scenarios/catalog.yml.
    class Catalog
      def self.load
        catalog_path = File.expand_path("../../scenarios/catalog.yml", __FILE__)
        YAML.load_file(catalog_path)
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
        @live_repo = nil
        @tags = []
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

      def live_repo(nwo)
        @live_repo = nwo
        self
      end

      def tags(*t)
        @tags = t.flatten
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
      def prepare(binary, profile_dir: nil)
        if @live_repo
          return prepare_live(binary, profile_dir: profile_dir)
        end

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

        cmd = [binary] + @args
        if profile_dir
          pdir = File.join(profile_dir, @name.to_s)
          FileUtils.mkdir_p(pdir)
          cmd += ["--profile", pdir]
        end

        Context.new(dir: dir, env: env, cmd: cmd,
                    server: server, scenario: self)
      end

      # Prepare a live repo scenario: shallow-clone the real repo and run
      # against its actual workflows. No stub server — hits real GitHub API.
      def prepare_live(binary, profile_dir: nil)
        dir = Dir.mktmpdir("actions-pin-live-")
        nwo = @live_repo

        $stderr.print "\e[2m  cloning #{nwo}…\e[0m "
        $stderr.flush
        system("git", "clone", "--depth=1", "--quiet",
               "https://github.com/#{nwo}.git", dir,
               exception: true)
        $stderr.puts "\e[2mdone\e[0m"

        env = @env.dup
        @setup_blocks.each { |b| b.call(dir) }
        env["GH_ACTIONS_PIN_CACHE_DIR"] = File.join(dir, ".cache")

        cmd = [binary] + @args
        if profile_dir
          pdir = File.join(profile_dir, @name.to_s)
          FileUtils.mkdir_p(pdir)
          cmd += ["--profile", pdir]
        end

        Context.new(dir: dir, env: env, cmd: cmd,
                    server: nil, scenario: self, live_repo: nwo)
      end

      # Batch mode: capture output, run assertions.
      def run(binary, profile_dir: nil)
        # Live repo scenarios need a token
        if @live_repo
          token = ENV["GH_TOKEN"] || ENV["GITHUB_TOKEN"] || ""
          raise SkipScenario, "no GH_TOKEN" if token.empty?
        end

        ctx = prepare(binary, profile_dir: profile_dir)
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
      attr_reader :dir, :env, :cmd, :server, :scenario, :live_repo

      def initialize(dir:, env:, cmd:, server:, scenario:, live_repo: nil)
        @dir = dir
        @env = env
        @cmd = cmd
        @server = server
        @scenario = scenario
        @live_repo = live_repo
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
              # Read in chunks so we flush once per available burst instead
              # of once per byte. Eliminates flicker while keeping spinners
              # responsive (readpartial returns as soon as data is available).
              $stdout.sync = true
              loop do
                chunk = reader.readpartial(4096)
                $stdout.write chunk
                combined << chunk
              end
            rescue Errno::EIO, EOFError
              # PTY closed — normal on macOS when process exits
            ensure
              $stdout.sync = false
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
      attr_accessor :profile_dir

      def initialize(binary: nil)
        @binary = binary || find_binary
        @scenarios = []
        @profile_dir = nil
        @pause = false
        @last_dir = nil
      end

      def scenario(name, &block)
        s = Scenario.new(name)
        block.call(s)
        @scenarios << s
        s
      end

      # ── Batch mode ─────────────────────────────────────────────────

      def run(filter: nil, tag_filter: nil, catalog: nil)
        to_run = @scenarios.dup

        # Name filter (positional arg)
        to_run = to_run.select { |s| s.name.to_s.include?(filter) } if filter

        # Tag filter (--live, --stub, --smoke, --real)
        if tag_filter && catalog
          tagged_names = catalog["scenarios"]
            .select { |cs| (cs["tags"] || []).include?(tag_filter) }
            .map { |cs| cs["name"] }
            .to_set

          to_run = to_run.select { |s| tagged_names.include?(s.name.to_s) }
        end

        puts "Running #{to_run.size} integration scenario(s)...\n\n"

        passed = 0
        failed = 0
        skipped = 0

        to_run.each do |s|
          print "  #{s.name} ... "
          begin
            s.run(@binary, profile_dir: @profile_dir)
            if s.failures.empty?
              puts "\e[32m✓\e[0m"
              passed += 1
            else
              puts "\e[31m✗\e[0m"
              s.failures.each { |f| puts "    \e[31m#{f}\e[0m" }
              failed += 1
            end
          rescue SkipScenario => e
            puts "\e[33m⊘ skip\e[0m #{e.message}"
            skipped += 1
          rescue => e
            puts "\e[31m✗ (exception)\e[0m"
            puts "    #{e.class}: #{e.message}"
            puts e.backtrace.first(5).map { |l| "    #{l}" }.join("\n")
            failed += 1
          end
        end

        parts = ["#{passed} passed", "#{failed} failed"]
        parts << "#{skipped} skipped" if skipped > 0
        puts "\n#{parts.join(', ')}"
        exit(failed > 0 ? 1 : 0)
      end

      # ── Matrix display ──────────────────────────────────────────────

      def print_matrix(catalog)
        puts "\e[1mScenario Matrix\e[0m\n\n"

        categories = catalog["categories"] || []
        scenarios = catalog["scenarios"] || []

        categories.each do |cat|
          cat_scenarios = scenarios.select { |s| s["category"] == cat["name"] }
          next if cat_scenarios.empty?

          puts "  \e[1m#{cat["name"]}\e[0m — #{cat["description"]}"
          cat_scenarios.each do |cs|
            tags = (cs["tags"] || []).map { |t| "\e[36m##{t}\e[0m" }.join(" ")
            mode = cs["needs_stub"] ? "\e[33mstub\e[0m" : cs["needs_token"] ? "\e[32mlive\e[0m" : "\e[90mnone\e[0m"
            registered = @scenarios.any? { |s| s.name.to_s == cs["name"] } ? "✓" : "✗"
            puts "    #{registered} \e[37m%-35s\e[0m [#{mode}] #{tags}" % cs["name"]
            puts "      #{cs['description']}"
          end
          puts
        end

        total = scenarios.size
        registered = @scenarios.size
        puts "#{registered}/#{total} scenarios registered"
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
        puts "  \e[36mdiff\e[0m                  Show git diff from last run"
        puts "  \e[36mcd <name>\e[0m             Prepare scenario and drop into its dir"
        puts "  \e[36mpause\e[0m                 Toggle pause between scenarios in run-all"
        puts "  \e[36mprofile [dir|off]\e[0m     Toggle profiling (default: ./profiles)"
        puts "  \e[36mquit\e[0m                  Exit"
        puts

        active_ctx = nil
        scenario_names = @scenarios.map { |s| s.name.to_s }
        commands = %w[list ls run test inspect diff cd rerun pause profile quit exit q]

        # Tab completion: commands first, then scenario names for run/test/inspect/cd
        Reline.completion_proc = proc do |input|
          line = Reline.line_buffer
          parts = line.split(/\s+/, 2)
          if parts.size >= 2 && %w[run test inspect cd].include?(parts[0])
            candidates = scenario_names + ["all"]
            candidates.select { |n| n.start_with?(input) }
          elsif parts.size <= 1
            (commands + scenario_names).select { |c| c.start_with?(input) }
          else
            []
          end
        end

        Reline.completion_append_character = " "

        loop do
          prompt = active_ctx ? "\e[33m#{active_ctx.scenario.name}\e[0m > " : "\e[35mpin-test\e[0m > "
          begin
            line = Reline.readline(prompt, true)
          rescue Interrupt
            puts
            next
          end
          break if line.nil?
          line = line.strip

          # Remove blank/duplicate entries from history
          if line.empty? || (Reline::HISTORY.size > 1 && Reline::HISTORY[-2] == line)
            Reline::HISTORY.pop
          end
          next if line.empty?

          parts = line.split(/\s+/, 2)
          verb = parts[0]
          arg  = parts[1]

          begin
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
            ctx = s.prepare(@binary, profile_dir: @profile_dir)
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
            ctx = s.prepare(@binary, profile_dir: @profile_dir)
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

          when "profile"
            if arg.nil? || arg == "on"
              @profile_dir = File.expand_path("profiles")
              FileUtils.mkdir_p(@profile_dir)
              puts "Profiling \e[32mon\e[0m → #{@profile_dir}"
            elsif arg == "off"
              @profile_dir = nil
              puts "Profiling \e[33moff\e[0m"
            else
              @profile_dir = File.expand_path(arg)
              FileUtils.mkdir_p(@profile_dir)
              puts "Profiling \e[32mon\e[0m → #{@profile_dir}"
            end

          when "pause"
            @pause = !@pause
            puts "Pause between scenarios: #{@pause ? "\e[32mon\e[0m" : "\e[33moff\e[0m"}"

          when "diff"
            dir = active_ctx&.dir || @last_dir
            if dir && Dir.exist?(dir)
              show_diff(dir)
            else
              puts "No scenario dir available. Run a scenario first."
            end

          else
            puts "Unknown command: #{verb}. Type \e[36mlist\e[0m for scenarios."
          end
          rescue Interrupt
            puts "\n  \e[33m⊘ interrupted\e[0m"
          end
        end

        active_ctx&.teardown
        puts "\nBye."
      end

      private

      def show_diff(dir)
        return unless dir && Dir.exist?(dir)
        diff = `cd #{Shellwords.shellescape(dir)} && git --no-pager diff --color 2>/dev/null`.strip
        return if diff.empty?
        puts
        puts "  \e[1m── diff ──\e[0m"
        diff.each_line { |l| puts "  #{l}" }
      end

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
        ctx = s.prepare(@binary, profile_dir: @profile_dir)
        keep = ENV["KEEP_FIXTURES"]
        begin
          result = ctx.run_pty
          puts
          if result.success?
            puts "  \e[32m✓ exit 0\e[0m"
          else
            puts "  \e[31m✗ exit #{result.exit_code}\e[0m"
          end
          if @profile_dir
            pdir = File.join(@profile_dir, s.name.to_s)
            puts "  \e[2mprofile: #{pdir}\e[0m"
          end
          show_diff(ctx.dir)
          @last_dir = ctx.dir
          puts
        ensure
          ctx.teardown unless keep
        end
      end

      def run_all_live
        @scenarios.each_with_index do |s, i|
          run_one_live(s)
          if @pause && i < @scenarios.size - 1
            print "\e[2m  press Enter to continue (q to stop)…\e[0m "
            input = $stdin.gets&.strip
            break if input&.start_with?("q")
          end
        end
      rescue Interrupt
        puts "\n  \e[33m⊘ interrupted\e[0m"
      end

      def run_batch(to_run)
        puts "Running #{to_run.size} scenario(s)...\n\n"
        passed = 0
        failed = 0

        to_run.each do |s|
          print "  #{s.name} ... "
          begin
            s.run(@binary, profile_dir: @profile_dir)
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
