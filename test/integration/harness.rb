# frozen_string_literal: true

# Integration test harness for gh-actions-lock.
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
        YAML.safe_load_file(catalog_path, permitted_classes: [Symbol])
      end
    end

    # ── Scenario ────────────────────────────────────────────────────────
    class Scenario
      attr_reader :name, :failures, :last_cmd, :tags, :last_diff
      attr_accessor :category, :description, :expect_spec, :fixture_spec, :input_spec, :skip_reason

      # Expose fixture state for display (read-only)
      def cli_args;        @args;      end
      def workflows;       @workflows; end
      def lockfile_content; @lockfile;  end
      def live_repo_nwo;   @live_repo; end

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
        @needs_token = false
        @tags = []
        @input_spec = nil
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

      def onboarded?
        !@lockfile.nil?
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
        @needs_token = true
        self
      end

      def needs_token(val = true)
        @needs_token = val
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

      def assert_lockfile_comment_matches(pattern)
        @assertions << -> (r) {
          lockpath = File.join(r.dir, ".github", "workflows", "actions.lock")
          content = File.read(lockpath) rescue ""
          assert_match("lockfile comment matches /#{pattern}/", content, Regexp.new(pattern))
        }
        self
      end

      def assert_lockfile_comment_excludes(pattern)
        @assertions << -> (r) {
          lockpath = File.join(r.dir, ".github", "workflows", "actions.lock")
          content = File.read(lockpath) rescue ""
          assert_no_match("lockfile comment excludes /#{pattern}/", content, Regexp.new(pattern))
        }
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

        dir = Dir.mktmpdir("actions-lock-test-")

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
          env["GH_ACTIONS_LOCK_INSECURE"] = "1"
        end

        @setup_blocks.each { |b| b.call(dir) }
        env["GH_ACTIONS_LOCK_CACHE_DIR"] = File.join(dir, ".cache")

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
        dir = Dir.mktmpdir("actions-lock-live-")
        nwo = @live_repo

        $stderr.print "\e[2m  cloning #{nwo}…\e[0m "
        $stderr.flush
        system("git", "clone", "--depth=1", "--quiet",
               "https://github.com/#{nwo}.git", dir,
               exception: true)
        $stderr.puts "\e[2mdone\e[0m"

        env = @env.dup
        @setup_blocks.each { |b| b.call(dir) }
        env["GH_ACTIONS_LOCK_CACHE_DIR"] = File.join(dir, ".cache")

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
        raise SkipScenario, @skip_reason if @skip_reason

        if @needs_token || @live_repo
          token = ENV["GH_TOKEN"] || ENV["GITHUB_TOKEN"]
          if token.nil? || token.empty?
            gh_token = `gh auth token 2>/dev/null`.strip
            raise SkipScenario, "no GH_TOKEN or gh auth" if gh_token.empty?
          end
        end

        ctx = prepare(binary, profile_dir: profile_dir)
        @last_cmd = ctx.cmd_string
        begin
          result = ctx.run_captured
          @last_diff = `cd #{Shellwords.shellescape(ctx.dir)} && git add -N . 2>/dev/null; git --no-pager diff --color 2>/dev/null`.strip
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
      #
      # input_prompts: optional array of {prompt:, response:} hashes.
      # When the accumulated output matches a prompt pattern, the
      # corresponding response is written to the PTY's stdin.
      def run_pty(input_prompts: nil)
        flat_env = @env.map { |k, v| "#{k}=#{Shellwords.shellescape(v)}" }
        shell_cmd = "cd #{Shellwords.shellescape(@dir)} && " +
                    flat_env.join(" ") + " " +
                    @cmd.map { |c| Shellwords.shellescape(c) }.join(" ")

        combined = String.new
        exit_code = nil
        prompts = (input_prompts || []).map { |p| p.dup }

        begin
          PTY.spawn("/bin/bash", "-c", shell_cmd) do |reader, writer, pid|
            begin
              prev_sync = $stdout.sync
              $stdout.sync = true
              loop do
                chunk = reader.readpartial(4096)
                $stdout.write chunk
                combined << chunk

                # Survey sends DSR (\e[6n]) to query cursor position. In a
                # real terminal the emulator replies with a CPR; in a PTY no
                # one does, so survey hangs in CursorLocation(). We play
                # terminal emulator: when \e[6n appears, write a CPR back.
                if chunk.include?("\e[6n")
                  writer.write("\e[1;1R")
                end

                # Check for pending prompts and auto-respond.
                # Survey runs in raw mode where Enter = \r, not \n.
                prompts.reject! do |p|
                  if combined.gsub(/\e\[[0-9;]*[a-zA-Z]/, "").include?(p["prompt"])
                    # Let survey finish rendering after CursorLocation.
                    loop do
                      ready = IO.select([reader], nil, nil, 0.3)
                      break unless ready
                      begin
                        extra = reader.readpartial(4096)
                        $stdout.write extra
                        combined << extra
                        writer.write("\e[1;1R") if extra.include?("\e[6n")
                      rescue Errno::EIO, EOFError
                        break
                      end
                    end
                    writer.write(p["response"] + "\r")
                    true
                  end
                end
              end
            rescue Errno::EIO, EOFError
              # PTY closed — normal on macOS when process exits
            ensure
              $stdout.sync = prev_sync
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
        @diff_cache = {}       # name → diff string
        @diff_order = []       # insertion order for eviction
      end

      def diff_cache_limit
        [20, @scenarios.size].max
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

      # ── Golden JSON capture ───────────────────────────────────────
      #
      # Runs scenarios in the given category, captures the literal
      # --json stdout, and writes it back into catalog.yml under
      # expect.golden_json. This makes the binary the source of truth
      # for the integration contract — hand-authored jq assertions
      # coexist for backward compatibility, but golden_json is the
      # exact body consumers can rely on.

      def golden_update(category:, catalog:)
        catalog_path = File.expand_path("../../scenarios/catalog.yml", __FILE__)
        cat_scenarios = catalog["scenarios"].select { |cs| cs["category"] == category }

        if cat_scenarios.empty?
          $stderr.puts "No scenarios in category #{category.inspect}"
          exit 1
        end

        eligible = cat_scenarios.reject { |cs| cs["skip"] }
        json_eligible = eligible.select { |cs| (cs["flags"] || []).any? { |f| f.start_with?("--json") } }

        puts "\e[1mGolden update: #{category}\e[0m"
        puts "  #{cat_scenarios.size} total, #{eligible.size} runnable, #{json_eligible.size} with --json"
        puts

        updated = 0
        failed = 0
        skipped = 0

        json_eligible.each do |cs|
          name = cs["name"]
          scenario = @scenarios.find { |s| s.name.to_s == name }
          unless scenario
            puts "  #{name} ... \e[33m⊘ not registered\e[0m"
            skipped += 1
            next
          end

          print "  #{name} ... "
          begin
            result = scenario.run(@binary, profile_dir: nil)
            stdout = result.stdout.strip

            parsed = JSON.parse(stdout)

            # Deep-sort keys for stable YAML output
            normalized = deep_sort_keys(parsed)

            cs["expect"] ||= {}
            cs["expect"]["golden_json"] = normalized

            puts "\e[32m✓ captured\e[0m"
            updated += 1
          rescue SkipScenario => e
            puts "\e[33m⊘ skip\e[0m #{e.message}"
            skipped += 1
          rescue JSON::ParserError => e
            # Exit 2 (engine error) scenarios produce no JSON — that's expected
            if result && result.exit_code == 2
              puts "\e[33m⊘ no JSON (exit 2)\e[0m"
              skipped += 1
            else
              puts "\e[31m✗ not JSON\e[0m — #{e.message}"
              failed += 1
            end
          rescue => e
            puts "\e[31m✗ error\e[0m — #{e.class}: #{e.message}"
            failed += 1
          end
        end

        # Write back the full catalog with updated golden_json blocks
        if updated > 0
          write_golden_catalog(catalog_path, catalog)
          puts "\n\e[32m#{updated} golden bodies written to catalog.yml\e[0m"
        end

        parts = ["#{updated} captured", "#{failed} failed"]
        parts << "#{skipped} skipped" if skipped > 0
        puts parts.join(", ")
        exit(failed > 0 ? 1 : 0)
      end

      private

      def deep_sort_keys(obj)
        case obj
        when Hash
          obj.sort.to_h.transform_values { |v| deep_sort_keys(v) }
        when Array
          obj.map { |v| deep_sort_keys(v) }
        else
          obj
        end
      end

      def write_golden_catalog(path, catalog)
        # Re-serialize the full catalog. Use block style for readability.
        yaml = YAML.dump(catalog)

        # YAML.dump wraps strings in quotes and uses flow style for small
        # arrays. The catalog is hand-authored with a specific style, so
        # we do a targeted update instead: for each scenario with
        # golden_json, find its expect block and insert/replace the
        # golden_json sub-block.
        lines = File.readlines(path)
        catalog["scenarios"].each do |cs|
          golden = cs.dig("expect", "golden_json")
          next unless golden

          # Find the scenario by name
          name_line_idx = lines.index { |l| l.strip == "- name: #{cs['name']}" }
          next unless name_line_idx

          # Find the expect: line within this scenario
          expect_idx = nil
          (name_line_idx + 1...lines.size).each do |i|
            break if lines[i] =~ /^\s{2}- name:/ && i > name_line_idx
            if lines[i] =~ /^\s+expect:\s*$/
              expect_idx = i
              break
            end
          end
          next unless expect_idx

          # Determine the indentation of the expect block's children
          expect_indent = lines[expect_idx][/^\s*/].length
          child_indent = expect_indent + 2

          # Find the end of the expect block (next sibling or next scenario)
          expect_end = lines.size
          (expect_idx + 1...lines.size).each do |i|
            # A line at the same or lesser indent that isn't blank → end
            if lines[i] =~ /\S/ && lines[i][/^\s*/].length <= expect_indent
              expect_end = i
              break
            end
          end

          # Check if golden_json already exists in the expect block
          golden_start = nil
          golden_end = nil
          (expect_idx + 1...expect_end).each do |i|
            if lines[i] =~ /^#{' ' * child_indent}golden_json:/
              golden_start = i
              # Find end of golden_json sub-block
              (i + 1...expect_end).each do |j|
                if lines[j] =~ /\S/ && lines[j][/^\s*/].length <= child_indent
                  golden_end = j
                  break
                end
                golden_end = j + 1
              end
              break
            end
          end

          # Format the golden_json as YAML lines
          golden_yaml = format_golden_yaml(golden, child_indent)

          if golden_start
            lines[golden_start...golden_end] = golden_yaml
          else
            # Insert before the end of the expect block
            insert_at = expect_end
            lines.insert(insert_at, *golden_yaml)
          end
        end

        File.write(path, lines.join)
      end

      def format_golden_yaml(obj, indent)
        prefix = " " * indent
        lines = ["#{prefix}golden_json:\n"]
        format_yaml_value(obj, indent + 2, lines)
        lines
      end

      def format_yaml_value(obj, indent, lines)
        prefix = " " * indent
        case obj
        when Hash
          obj.each do |k, v|
            case v
            when Hash, Array
              lines << "#{prefix}#{k}:\n"
              format_yaml_value(v, indent + 2, lines)
            else
              lines << "#{prefix}#{k}: #{yaml_scalar(v)}\n"
            end
          end
        when Array
          if obj.empty?
            # Replace the last line's trailing newline with " []\n"
            lines[-1] = lines[-1].chomp + " []\n"
          else
            obj.each do |item|
              if item.is_a?(Hash)
                first = true
                item.each do |k, v|
                  item_prefix = first ? "#{prefix}- " : "#{prefix}  "
                  first = false
                  case v
                  when Hash, Array
                    lines << "#{item_prefix}#{k}:\n"
                    format_yaml_value(v, indent + 4, lines)
                  else
                    lines << "#{item_prefix}#{k}: #{yaml_scalar(v)}\n"
                  end
                end
              else
                lines << "#{prefix}- #{yaml_scalar(item)}\n"
              end
            end
          end
        end
      end

      def yaml_scalar(v)
        case v
        when true then "true"
        when false then "false"
        when nil then "null"
        when Integer, Float then v.to_s
        when String
          # Quote strings that could be misinterpreted
          if v.empty? || v =~ /^[\{\[\d]/ || v =~ /[:#]/ || %w[true false null yes no].include?(v.downcase)
            v.inspect
          else
            v
          end
        else
          v.inspect
        end
      end

      public

      # ── Interactive shell ──────────────────────────────────────────

      def shell
        puts "\e[1mgh-actions-lock integration shell\e[0m"
        puts "Binary: #{@binary}"
        puts "Scenarios: #{@scenarios.size} loaded"
        puts "Type \e[36mhelp\e[0m for commands, \e[36mlist\e[0m for scenarios."
        puts

        active_ctx = nil
        scenario_names = @scenarios.map { |s| s.name.to_s }
        commands = %w[list ls run test review inspect diff cd rerun build pause profile auth status clear help quit exit q]

        # Tab completion: commands first, then scenario names for run/test/inspect/cd
        Reline.completion_proc = proc do |input|
          line = Reline.line_buffer
          parts = line.split(/\s+/, 2)
          if parts.size >= 2 && %w[run test review inspect cd diff].include?(parts[0])
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
            grouped = @scenarios.group_by { |s| s.respond_to?(:category) ? s.category : "other" }
            grouped.each do |cat, scenarios|
              puts "  \e[1m#{cat}\e[0m"
              scenarios.each { |s| puts "    \e[36m#{s.name}\e[0m" }
            end

          when "run"
            active_ctx&.teardown
            active_ctx = nil

            if arg == "all"
              run_all_live
            elsif arg && arg.start_with?("cat:")
              cat = arg.sub("cat:", "")
              to_run = @scenarios.select { |s| s.respond_to?(:category) && s.category == cat }
              if to_run.empty?
                puts "No scenarios in category '#{cat}'. Try \e[36mlist\e[0m."
              else
                to_run.each_with_index do |s, i|
                  run_one_live(s)
                  if @pause && i < to_run.size - 1
                    print "\e[2m  press Enter to continue (q to stop)…\e[0m "
                    input = $stdin.gets&.strip
                    break if input&.start_with?("q")
                  end
                end
              end
            elsif arg && repo_nwo?(arg.split(/\s+--\s+/, 2)[0])
              nwo, extra = split_adhoc_args(arg)
              active_ctx = run_one_live(adhoc_scenario(nwo, extra_args: extra), keep_alive: true)
            else
              s = find_scenario(arg)
              next unless s
              run_one_live(s)
            end

          when "test"
            if arg && arg.start_with?("cat:")
              cat = arg.sub("cat:", "")
              to_run = @scenarios.select { |s| s.respond_to?(:category) && s.category == cat }
            else
              to_run = arg ? @scenarios.select { |s| s.name.to_s.include?(arg) } : @scenarios
            end
            run_batch(to_run)

          when "review"
            if arg && arg.start_with?("cat:")
              cat = arg.sub("cat:", "")
              to_run = @scenarios.select { |s| s.respond_to?(:category) && s.category == cat }
            elsif arg == "all" || arg.nil?
              to_run = @scenarios
            else
              to_run = @scenarios.select { |s| s.name.to_s.include?(arg) }
            end
            if to_run.empty?
              puts "No scenarios to review."
            else
              run_review(to_run)
            end

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
              w = 62
              puts "\e[1;36m── re-running #{active_ctx.scenario.name} ──\e[0m"
              puts "\e[2m$\e[0m #{active_ctx.cmd_string}"
              puts
              t0 = Process.clock_gettime(Process::CLOCK_MONOTONIC)
              result = active_ctx.run_pty(input_prompts: active_ctx.scenario.input_spec)
              elapsed = Process.clock_gettime(Process::CLOCK_MONOTONIC) - t0
              puts

              diff_text = `cd #{Shellwords.shellescape(active_ctx.dir)} && git add -N . 2>/dev/null; git --no-pager diff --color 2>/dev/null`.strip
              if diff_text.empty?
                puts "\e[1;35m── DIFF #{"─" * (w - 8)}\e[0m"
                puts "  \e[32m✓ no changes from previous run\e[0m"
                puts
              else
                cache_diff(active_ctx.scenario.name.to_s, diff_text)
                show_diff(active_ctx.dir, w, scenario_name: active_ctx.scenario.name.to_s)
              end

              # Checkpoint so the next rerun diff is also a delta
              system("cd #{Shellwords.shellescape(active_ctx.dir)} && git add -A && git commit -q --allow-empty -m rerun-state >/dev/null 2>&1")

              if @profile_dir
                pdir = File.join(@profile_dir, active_ctx.scenario.name.to_s)
                puts "  \e[2mprofile: #{pdir}\e[0m"
              end
              puts
              status_color = result.exit_code == 0 ? "42" : "41"
              status_icon  = result.exit_code == 0 ? "✓ PASS" : "✗ FAIL"
              puts "\e[#{status_color};1;37m  #{status_icon}  \e[0m  exit #{result.exit_code}  \e[2m(#{format_elapsed(elapsed)})\e[0m"
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
            show_paged_diff(arg)

          when "auth"
            show_auth

          when "clear"
            print "\033[2J\033[H"

          when "build"
            repo_root = File.expand_path("../..", __dir__)
            print "  \e[2mbuilding…\e[0m "
            $stdout.flush
            t0 = Process.clock_gettime(Process::CLOCK_MONOTONIC)
            ok = system("go", "build", "-o", "gh-actions-lock", "./cmd/gh-actions-lock", chdir: repo_root)
            elapsed = Process.clock_gettime(Process::CLOCK_MONOTONIC) - t0
            if ok
              puts "\e[32m✓\e[0m \e[2m(#{format_elapsed(elapsed)})\e[0m"
              @binary = File.join(repo_root, "gh-actions-lock")
            else
              puts "\e[31m✗ build failed\e[0m"
            end

          when "help", "?"
            print_help

          when "status"
            parts = []
            parts << "pause: #{@pause ? "\e[32mon\e[0m" : "\e[33moff\e[0m"}"
            parts << "profile: #{@profile_dir ? "\e[32m#{@profile_dir}\e[0m" : "\e[33moff\e[0m"}"
            parts << "active: #{active_ctx ? "\e[36m#{active_ctx.scenario.name}\e[0m" : "\e[90mnone\e[0m"}"
            puts "  " + parts.join("  ")

          else
            # Bare scenario name or owner/repo → run it directly
            if repo_nwo?(verb)
              active_ctx&.teardown
              active_ctx = nil
              extra = if arg && arg.match?(/\A--\s/)
                        arg.sub(/\A--\s+/, "").split(/\s+/)
                      else
                        []
                      end
              run_one_live(adhoc_scenario(verb, extra_args: extra))
            else
              s = @scenarios.find { |sc| sc.name.to_s == verb }
              if s
                active_ctx&.teardown
                active_ctx = nil
                run_one_live(s)
              else
                puts "Unknown command: #{verb}. Type \e[36mhelp\e[0m for commands."
              end
            end
          end
          rescue Interrupt
            puts "\n  \e[33m⊘ interrupted\e[0m"
          end
        end

        active_ctx&.teardown
        puts "\nBye."
      end

      private

      MAX_DIFF_LINES = 30

      def cache_diff(name, diff_text)
        return if diff_text.nil? || diff_text.empty?
        @diff_cache.delete(name)
        @diff_order.delete(name)
        @diff_cache[name] = diff_text
        @diff_order << name
        while @diff_order.size > diff_cache_limit
          evict = @diff_order.shift
          @diff_cache.delete(evict)
        end
      end

      def show_paged_diff(name)
        diff = name ? @diff_cache[name] : @diff_cache[@diff_order.last]
        if diff.nil? || diff.empty?
          if name
            puts "\e[2m  no diff cached for '#{name}'\e[0m"
            available = @diff_order.select { |n| !@diff_cache[n].empty? }
            puts "  \e[2mavailable: #{available.join(", ")}\e[0m" if available.any?
          else
            puts "\e[2m  no diff available — run a scenario first\e[0m"
          end
          return
        end
        IO.popen(["less", "-R"], "w") { |io| io.write(diff) }
      rescue Errno::EPIPE
        # user quit pager early — that's fine
      end

      def show_starting_state(dir, width)
        inner = width - 2
        has_content = false

        # Show lockfile if present
        lockfile = File.join(dir, ".github", "workflows", "actions.lock")
        if File.exist?(lockfile)
          content = File.read(lockfile)
          # Strip the header comments, show the meat
          lines = content.lines.reject { |l| l.start_with?("#") || l.strip.empty? }
          # Show dep keys with their refs for quick understanding
          deps = []
          lines.each do |l|
            if l =~ /^\s+'?([^:]+@[^:]+):sha1-/
              deps << $1
            end
          end
          if deps.any?
            puts "\e[1;35m── STARTING STATE #{"─" * (width - 19)}\e[0m"
            has_content = true
            puts "  \e[2mlockfile deps:\e[0m"
            deps.each { |d| puts "    \e[33m#{d}\e[0m" }
          end
        end

        # Show workflow action refs
        wf_dir = File.join(dir, ".github", "workflows")
        if Dir.exist?(wf_dir)
          wf_actions = {}
          Dir.glob("#{wf_dir}/*.yml").each do |f|
            next if File.basename(f) == "actions.lock"
            File.readlines(f).each do |line|
              if line =~ /uses:\s*(\S+@\S+)/
                (wf_actions[File.basename(f)] ||= []) << $1
              end
            end
          end
          if wf_actions.any?
            unless has_content
              puts "\e[1;35m── STARTING STATE #{"─" * (width - 19)}\e[0m"
              has_content = true
            end
            puts "  \e[2mworkflow refs:\e[0m"
            wf_actions.each do |file, actions|
              actions.each { |a| puts "    \e[36m#{file}\e[0m → #{a}" }
            end
          end
        end

        puts if has_content
      end

      def show_diff(dir, width, scenario_name: nil)
        return unless dir && Dir.exist?(dir)
        diff = `cd #{Shellwords.shellescape(dir)} && git add -N . 2>/dev/null; git --no-pager diff --color 2>/dev/null`.strip
        return if diff.empty?
        lines = diff.lines
        puts
        puts "\e[1;35m── DIFF #{"─" * (width - 9)}\e[0m"
        if lines.size <= MAX_DIFF_LINES
          lines.each { |l| puts "  #{l}" }
        else
          lines.first(MAX_DIFF_LINES).each { |l| puts "  #{l}" }
          omitted = lines.size - MAX_DIFF_LINES
          hint = scenario_name ? "diff #{scenario_name}" : "diff"
          puts "  \e[2m… #{omitted} more lines truncated\e[0m"
          puts "  \e[2m→ \e[0m\e[36m#{hint}\e[0m"
        end
      end

      def print_help
        puts "Commands:"
        puts "  \e[36mlist\e[0m                  Show all scenarios (grouped by category)"
        puts "  \e[36mrun <name>\e[0m            Run scenario with live PTY output"
        puts "  \e[36mrun cat:<category>\e[0m   Run all scenarios in a category"
        puts "  \e[36mrun <owner/repo>\e[0m     Run ad-hoc against any GitHub repo"
        puts "  \e[36mrun all\e[0m               Run all scenarios with live output"
        puts "  \e[36m<name>\e[0m                Run scenario directly (shorthand for run)"
        puts "  \e[36m<owner/repo>\e[0m          Run ad-hoc against any GitHub repo"
        puts "  \e[36mtest [filter]\e[0m         Batch-test scenarios (captured, assertions)"
        puts "  \e[36mreview [filter]\e[0m       Interactive review: pass/flag/skip + report"
        puts "  \e[36minspect <name>\e[0m        Show scenario fixtures without running"
        puts "  \e[36mdiff\e[0m                  Show full diff from last run (pager)"
        puts "  \e[36mdiff <name>\e[0m           Show cached diff for a specific scenario"
        puts "  \e[36mcd <name>\e[0m             Prepare scenario and drop into its dir"
        puts "  \e[36mrerun\e[0m                 Re-run active scenario (keeps lockfile state)"
        puts "  \e[36mbuild\e[0m                 Rebuild the binary (go build)"
        puts "  \e[36mpause\e[0m                 Toggle pause between scenarios in run-all"
        puts "  \e[36mprofile [dir|off]\e[0m     Toggle profiling (default: ./profiles)"
        puts "  \e[36mauth\e[0m                  Show current auth source"
        puts "  \e[36mstatus\e[0m                Show current toggles"
        puts "  \e[36mclear\e[0m                 Clear screen"
        puts "  \e[36mhelp\e[0m                  Show this help"
        puts "  \e[36mquit\e[0m                  Exit (or Ctrl+D)"
      end

      def show_auth
        gh_token = ENV["GH_TOKEN"]
        github_token = ENV["GITHUB_TOKEN"]
        gh_cli = `gh auth token 2>/dev/null`.strip
        gh_user = `gh auth status 2>&1`.lines.grep(/Logged in/).first&.strip

        if gh_token && !gh_token.empty?
          puts "  \e[32mGH_TOKEN\e[0m env var set (#{gh_token[0..7]}…)"
        elsif github_token && !github_token.empty?
          puts "  \e[32mGITHUB_TOKEN\e[0m env var set (#{github_token[0..7]}…)"
        elsif !gh_cli.empty?
          puts "  \e[32mgh CLI\e[0m auth (#{gh_cli[0..7]}…)"
          puts "  #{gh_user}" if gh_user
        else
          puts "  \e[31mno auth\e[0m — live scenarios will be skipped"
          puts "  Set GH_TOKEN or run: gh auth login"
        end
      end

      def format_elapsed(seconds)
        if seconds < 1
          "#{(seconds * 1000).round}ms"
        elsif seconds < 60
          "%.1fs" % seconds
        else
          "%dm%02ds" % [seconds / 60, seconds % 60]
        end
      end

      def find_binary
        repo_bin = File.expand_path("../../../gh-actions-lock", __FILE__)
        return repo_bin if File.executable?(repo_bin)

        gobin = File.join(ENV["HOME"], "go", "bin", "gh-actions-lock")
        return gobin if File.executable?(gobin)

        raise "Cannot find gh-actions-lock binary. Run `make build` first."
      end

      def find_scenario(name)
        unless name
          puts "Usage: run <scenario-name>"
          return nil
        end
        # Prefer exact match, fall back to substring.
        exact = @scenarios.find { |s| s.name.to_s == name }
        return exact if exact

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

      # Build an ad-hoc live Scenario for any owner/repo.
      def adhoc_scenario(nwo, extra_args: [])
        s = Scenario.new(:"adhoc_#{nwo.tr('/', '_')}")
        s.live_repo(nwo)
        s.args(*extra_args) unless extra_args.empty?
        s.category = "adhoc"
        s.description = nwo
        s
      end

      # Split "github/foo -- --no-fix --json" into ["github/foo", ["--no-fix", "--json"]]
      def split_adhoc_args(arg)
        parts = arg.split(/\s+--\s+/, 2)
        nwo = parts[0].strip
        extra = parts[1] ? parts[1].split(/\s+/) : []
        [nwo, extra]
      end

      def repo_nwo?(str)
        str.match?(%r{\A[A-Za-z0-9._-]+/[A-Za-z0-9._-]+\z})
      end

      def run_one_live(s, keep_alive: false)
        w = 62

        # ── TITLE ──
        label = " #{s.name} "
        inner = w - 2
        puts "\e[1;36m╔#{"═" * inner}╗\e[0m"
        puts "\e[1;36m║\e[1;37m#{label.center(inner)}\e[1;36m║\e[0m"
        puts "\e[1;36m╚#{"═" * inner}╝\e[0m"
        puts "  \e[2m#{s.description}\e[0m" if s.description
        puts

        # Prepare fixtures early so we can show starting state
        ctx = s.prepare(@binary, profile_dir: @profile_dir)

        # ── INPUT ──
        puts "\e[1m┌─ INPUT #{"─" * (w - 10)}┐\e[0m"
        lockfile_label = if s.onboarded?
                           tpl = s.fixture_spec&.dig("lockfile_template")
                           tpl ? "\e[32m● onboarded\e[0m \e[2m(#{tpl})\e[0m" : "\e[32m● onboarded\e[0m"
                         else
                           "\e[33m○ fresh\e[0m \e[2m(no lockfile)\e[0m"
                         end
        puts "\e[1m│\e[0m  lockfile: #{lockfile_label}"
        wf_specs = s.fixture_spec&.dig("workflows") || {}
        if s.live_repo_nwo
          puts "\e[1m│\e[0m  repo:     \e[36m#{s.live_repo_nwo}\e[0m"
        elsif wf_specs.any?
          wf_specs.each do |path, spec|
            actions = spec.is_a?(Hash) ? (spec["actions"] || []).join(", ") : "raw"
            puts "\e[1m│\e[0m  workflow: \e[36m#{path}\e[0m → #{actions}"
          end
        end
        flags = s.cli_args
        puts "\e[1m│\e[0m  flags:    #{flags.empty? ? "\e[2m(none)\e[0m" : flags.join(" ")}"
        if s.input_spec && !s.input_spec.empty?
          puts "\e[1m│\e[0m  input:    \e[33m#{s.input_spec.map { |p| "#{p["prompt"]}→#{p["response"]}" }.join(", ")}\e[0m"
        end
        puts "\e[1m└#{"─" * inner}┘\e[0m"
        puts

        # ── STARTING STATE ──
        show_starting_state(ctx.dir, w)

        # ── EXPECT ──
        if s.expect_spec && !s.expect_spec.empty?
          puts "\e[1m┌─ EXPECT #{"─" * (w - 11)}┐\e[0m"
          format_expect_lines(s.expect_spec).each { |line| puts "\e[1m│\e[0m  #{line}" }
          puts "\e[1m└#{"─" * inner}┘\e[0m"
          puts
        end

        # ── OUTPUT ──
        puts "\e[1;35m── OUTPUT #{"─" * (w - 10)}\e[0m"
        puts "\e[2m$\e[0m #{ctx.cmd_string}"
        puts
        keep = ENV["KEEP_FIXTURES"] || keep_alive
        t0 = Process.clock_gettime(Process::CLOCK_MONOTONIC)
        begin
          result = ctx.run_pty(input_prompts: s.input_spec)
          elapsed = Process.clock_gettime(Process::CLOCK_MONOTONIC) - t0
          puts

          # Capture full diff before teardown
          diff_text = `cd #{Shellwords.shellescape(ctx.dir)} && git add -N . 2>/dev/null; git --no-pager diff --color 2>/dev/null`.strip
          cache_diff(s.name.to_s, diff_text)

          # ── DIFF ──
          show_diff(ctx.dir, w, scenario_name: s.name.to_s)

          # ── RESULT ──
          s.instance_variable_set(:@failures, [])
          s.instance_variable_get(:@assertions).each { |a| a.call(result) }

          puts
          puts "\e[1;35m── RESULT #{"─" * (w - 10)}\e[0m"
          if s.expect_spec && !s.expect_spec.empty?
            format_expect_checks(s.expect_spec, result, s.failures).each { |line| puts line }
          end
          if @profile_dir
            pdir = File.join(@profile_dir, s.name.to_s)
            puts "  \e[2mprofile: #{pdir}\e[0m"
          end
          puts
          if s.failures.empty?
            puts "\e[42;1;37m  ✓ PASS  \e[0m  exit #{result.exit_code}  \e[2m(#{format_elapsed(elapsed)})\e[0m"
          else
            puts "\e[41;1;37m  ✗ FAIL  \e[0m  exit #{result.exit_code}  \e[2m(#{format_elapsed(elapsed)})\e[0m"
            uncovered = s.failures.select { |f|
              !f.include?("lockfile") && !f.include?("exit") && !f.include?("output") && !f.include?("stdout")
            }
            uncovered.each { |f| puts "    \e[31m▸ #{f}\e[0m" }
          end
          @last_dir = ctx.dir
          puts
          if keep_alive
            # Checkpoint working tree so rerun diff shows only the delta
            system("cd #{Shellwords.shellescape(ctx.dir)} && git add -A && git commit -q --allow-empty -m pin-state >/dev/null 2>&1")
            return ctx
          end
        ensure
          ctx.teardown unless keep
        end
        nil
      end

      def format_expect(spec)
        parts = []
        parts << "exit=#{spec['exit']}" if spec["exit"]
        parts << "exit∈#{spec['exit_any'].inspect}" if spec["exit_any"]
        parts << "output⊃#{spec['output_contains'].inspect}" if spec["output_contains"]
        parts << "output⊅#{spec['output_excludes'].inspect}" if spec["output_excludes"]
        parts << "stdout=json" if spec["stdout_is_json"]
        parts << "stdout⊃#{spec['stdout_contains'].inspect}" if spec["stdout_contains"]
        parts << "stdout⊅#{spec['stdout_excludes'].inspect}" if spec["stdout_excludes"]
        parts << "lockfile⊃/#{spec['lockfile_comment_matches']}/" if spec["lockfile_comment_matches"]
        parts << "lockfile⊅/#{spec['lockfile_comment_excludes']}/" if spec["lockfile_comment_excludes"]
        parts.join("  ")
      end

      def format_expect_lines(spec)
        lines = []
        lines << "exit = #{spec['exit']}" if spec["exit"]
        lines << "exit ∈ #{spec['exit_any'].inspect}" if spec["exit_any"]
        if spec["output_contains"]
          spec["output_contains"].each { |p| lines << "output contains #{p.inspect}" }
        end
        if spec["output_excludes"]
          spec["output_excludes"].each { |p| lines << "output excludes #{p.inspect}" }
        end
        lines << "stdout is valid JSON" if spec["stdout_is_json"]
        if spec["stdout_contains"]
          spec["stdout_contains"].each { |p| lines << "stdout contains #{p.inspect}" }
        end
        if spec["stdout_excludes"]
          spec["stdout_excludes"].each { |p| lines << "stdout excludes #{p.inspect}" }
        end
        lines << "lockfile matches /#{spec['lockfile_comment_matches']}/" if spec["lockfile_comment_matches"]
        lines << "lockfile excludes /#{spec['lockfile_comment_excludes']}/" if spec["lockfile_comment_excludes"]
        lines << "lockfile exists" if spec["lockfile_exists"]
        if spec["jq"]
          spec["jq"].each do |check|
            op = if check.key?("equals")
                   "== #{check['equals'].inspect}"
                 elsif check.key?("contains")
                   "contains #{check['contains'].inspect}"
                 elsif check.key?("not_equals")
                   "!= #{check['not_equals'].inspect}"
                 elsif check.key?("matches")
                   "=~ /#{check['matches']}/"
                 elsif check.key?("gt")
                   "> #{check['gt']}"
                 else
                   "exists"
                 end
            lines << "jq '#{check['expr']}' #{op}"
          end
        end
        lines
      end

      def format_expect_checks(spec, result, failures)
        lines = []
        checks = []

        # Build list of [label, passed?] checks
        if spec["exit"]
          checks << ["exit = #{spec['exit']} (got #{result.exit_code})", result.exit_code == spec["exit"]]
        end
        if spec["exit_any"]
          checks << ["exit ∈ #{spec['exit_any'].inspect} (got #{result.exit_code})", spec["exit_any"].include?(result.exit_code)]
        end
        if spec["output_contains"]
          spec["output_contains"].each do |pat|
            ok = result.output.include?(pat)
            checks << ["output contains #{pat.inspect}", ok]
          end
        end
        if spec["output_excludes"]
          spec["output_excludes"].each do |pat|
            ok = !result.output.include?(pat)
            checks << ["output excludes #{pat.inspect}", ok]
          end
        end
        if spec["stdout_is_json"]
          ok = begin; JSON.parse(result.stdout); true; rescue; false; end
          checks << ["stdout is valid JSON", ok]
        end
        if spec["stdout_contains"]
          spec["stdout_contains"].each do |pat|
            ok = result.stdout.include?(pat)
            checks << ["stdout contains #{pat.inspect}", ok]
          end
        end
        if spec["stdout_excludes"]
          spec["stdout_excludes"].each do |pat|
            ok = !result.stdout.include?(pat)
            checks << ["stdout excludes #{pat.inspect}", ok]
          end
        end
        if spec["lockfile_comment_matches"]
          pat = spec["lockfile_comment_matches"]
          ok = !failures.any? { |f| f.include?("lockfile comment matches") }
          checks << ["lockfile matches /#{pat}/", ok]
        end
        if spec["lockfile_comment_excludes"]
          pat = spec["lockfile_comment_excludes"]
          ok = !failures.any? { |f| f.include?("lockfile comment excludes") }
          checks << ["lockfile excludes /#{pat}/", ok]
        end
        if spec["lockfile_exists"]
          ok = !failures.any? { |f| f.include?("lockfile exists") }
          checks << ["lockfile exists", ok]
        end
        if spec["jq"]
          spec["jq"].each do |check|
            expr = check["expr"]
            ok = !failures.any? { |f| f.include?("jq '#{expr}'") }
            op = if check.key?("equals")
                   "== #{check['equals'].inspect}"
                 elsif check.key?("contains")
                   "contains #{check['contains'].inspect}"
                 elsif check.key?("not_equals")
                   "!= #{check['not_equals'].inspect}"
                 elsif check.key?("matches")
                   "=~ /#{check['matches']}/"
                 elsif check.key?("gt")
                   "> #{check['gt']}"
                 else
                   "exists"
                 end
            # Also show the actual value for richer feedback
            actual = nil
            begin
              parsed = JSON.parse(result.stdout)
              actual = IO.popen(["jq", "-r", expr], "r+") { |io| io.write(JSON.generate(parsed)); io.close_write; io.read }&.strip
            rescue
            end
            label = "jq '#{expr}' #{op}"
            label += " \e[2m(got #{actual})\e[0m" if actual && !ok
            checks << [label, ok]
          end
        end

        checks.each do |label, passed|
          icon = passed ? "\e[32m✓\e[0m" : "\e[31m✗\e[0m"
          lines << "  #{icon} #{label}"
        end
        lines
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

      def run_review(to_run)
        verdicts = []  # [{name:, verdict:, note:}]
        puts "\e[1;35m┌─ REVIEW MODE #{"─" * 47}┐\e[0m"
        puts "\e[1;35m│\e[0m  #{to_run.size} scenarios."
        puts "\e[1;35m│\e[0m  \e[32m[p]ass\e[0m \e[33m[f]lag\e[0m \e[1;33m[F]lag+share\e[0m \e[34m[d]iff\e[0m \e[36m[r]erun\e[0m \e[36m[j]ump\e[0m \e[2m[s]kip\e[0m \e[31m[q]uit\e[0m"
        puts "\e[1;35m│\e[0m  \e[1;33mF\e[0m = flag + upload partial gist   \e[36mj <name|#>\e[0m = jump to scenario"
        puts "\e[1;35m└#{"─" * 60}┘\e[0m"
        puts

        quit = false
        seen = {}  # name -> true, tracks which scenarios have a verdict
        i = 0
        while i < to_run.size
          s = to_run[i]
          run_one_live(s)
          puts
          loop do
            print "\e[1m  [#{i + 1}/#{to_run.size}]\e[0m \e[32mp\e[0m \e[33mf\e[0m \e[1;33mF\e[0m \e[34md\e[0m \e[36mr\e[0m \e[36mj\e[0m \e[2ms\e[0m \e[31mq\e[0m > "
            raw = $stdin.gets&.strip
            case raw
            when "d", "D", "diff"
              cached = @diff_cache[s.name.to_s]
              if cached && !cached.empty?
                show_paged_diff(s.name.to_s)
              else
                puts "    \e[2mno diff captured for this scenario\e[0m"
              end
              next
            when "r", "R", "rerun"
              puts "    \e[36m↻ re-running #{s.name}\e[0m"
              puts
              break  # don't increment i — re-runs same scenario
            when /\Aj(?:\s+(.+))?\z/i
              target = $1&.strip
              if target.nil? || target.empty?
                puts "    \e[2mscenarios:\e[0m"
                to_run.each_with_index do |sc, idx|
                  marker = seen[sc.name.to_s] ? "\e[2m✓\e[0m" : " "
                  puts "      #{marker} #{idx + 1}. #{sc.name}"
                end
                next
              end
              jump_idx = if target.match?(/\A\d+\z/)
                           target.to_i - 1
                         else
                           to_run.index { |sc| sc.name.to_s.include?(target) }
                         end
              if jump_idx && jump_idx >= 0 && jump_idx < to_run.size
                # Only mark forward-skipped scenarios that don't already have a verdict
                if jump_idx > i
                  ((i + 1)...jump_idx).each do |skip_i|
                    name = to_run[skip_i].name.to_s
                    next if seen[name]
                    verdicts << { name: name, verdict: :skipped, note: "jumped" }
                    seen[name] = true
                  end
                end
                i = jump_idx
                puts "    \e[36m→ jumping to #{to_run[i].name}\e[0m"
                puts
                break
              else
                puts "    \e[31mno match for '#{target}'\e[0m"
                next
              end
            when "q", "Q", "quit"
              name = s.name.to_s
              unless seen[name]
                verdicts << { name: name, verdict: :skipped, note: nil }
                seen[name] = true
              end
              to_run[(i + 1)..].each do |r|
                rname = r.name.to_s
                next if seen[rname]
                verdicts << { name: rname, verdict: :skipped, note: nil }
                seen[rname] = true
              end
              quit = true
              break
            when "F"
              print "    \e[33mnote:\e[0m "
              note = $stdin.gets&.strip
              verdicts << { name: s.name.to_s, verdict: :flagged, note: note }
              seen[s.name.to_s] = true
              upload_partial_report(verdicts, to_run.size)
              i += 1
              break
            when "f", "flag"
              print "    \e[33mnote:\e[0m "
              note = $stdin.gets&.strip
              verdicts << { name: s.name.to_s, verdict: :flagged, note: note }
              seen[s.name.to_s] = true
              i += 1
              break
            when "s", "S", "skip"
              verdicts << { name: s.name.to_s, verdict: :skipped, note: nil }
              seen[s.name.to_s] = true
              i += 1
              break
            else
              verdicts << { name: s.name.to_s, verdict: :passed, note: nil }
              seen[s.name.to_s] = true
              i += 1
              break
            end
          end
          break if quit
          puts
        end

        # Print report
        print_review_report(verdicts)
      rescue Interrupt
        puts "\n  \e[33m⊘ interrupted\e[0m"
      end

      def print_review_report(verdicts)
        passed  = verdicts.select { |v| v[:verdict] == :passed }
        flagged = verdicts.select { |v| v[:verdict] == :flagged }
        skipped = verdicts.select { |v| v[:verdict] == :skipped }

        puts
        puts "\e[1;35m╔════════════════════════════════════════════════════════════╗\e[0m"
        puts "\e[1;35m║\e[1;37m#{"REVIEW REPORT".center(58)}\e[1;35m║\e[0m"
        puts "\e[1;35m╚════════════════════════════════════════════════════════════╝\e[0m"
        puts
        puts "  \e[32m✓ #{passed.size} passed\e[0m  \e[33m⚑ #{flagged.size} flagged\e[0m  \e[2m⊘ #{skipped.size} skipped\e[0m"
        puts

        if flagged.any?
          puts "\e[1;33m  Flagged:\e[0m"
          flagged.each do |v|
            puts "    \e[33m⚑\e[0m #{v[:name]}"
            puts "      \e[2m#{v[:note]}\e[0m" if v[:note] && !v[:note].empty?
          end
          puts
        end

        if skipped.any?
          puts "\e[2m  Skipped: #{skipped.map { |v| v[:name] }.join(", ")}\e[0m"
          puts
        end

        # Write report file
        report_path = "/tmp/actions-lock-review-#{Time.now.strftime('%Y%m%d-%H%M%S')}.md"
        File.write(report_path, render_review_markdown(verdicts))
        puts "  \e[2mReport saved:\e[0m \e[36m#{report_path}\e[0m"
        puts

        # Offer gist upload
        print "  \e[34mUpload as gist?\e[0m \e[2m[y/N]\e[0m > "
        answer = $stdin.gets&.strip&.downcase
        if answer == "y"
          desc = "gh-actions-lock review #{Time.now.strftime('%Y-%m-%d %H:%M')}"
          out = `gh gist create #{Shellwords.shellescape(report_path)} --desc #{Shellwords.shellescape(desc)} 2>&1`.strip
          if $?.success?
            puts "  \e[32m✓\e[0m #{out}"
          else
            puts "  \e[31m✗ gist create failed:\e[0m #{out}"
          end
        end
      end

      def upload_partial_report(verdicts, total)
        reviewed = verdicts.size
        remaining = total - reviewed
        md = render_review_markdown(verdicts)
        md += "\n---\n_Partial report: #{reviewed}/#{total} reviewed, #{remaining} remaining._\n"
        path = "/tmp/actions-lock-review-partial-#{Time.now.strftime('%Y%m%d-%H%M%S')}.md"
        File.write(path, md)
        desc = "gh-actions-lock review (partial #{reviewed}/#{total}) #{Time.now.strftime('%Y-%m-%d %H:%M')}"
        out = `gh gist create #{Shellwords.shellescape(path)} --desc #{Shellwords.shellescape(desc)} 2>&1`.strip
        if $?.success?
          puts "    \e[32m✓ shared:\e[0m #{out}"
        else
          puts "    \e[31m✗ gist failed:\e[0m #{out}"
          puts "    \e[2msaved locally: #{path}\e[0m"
        end
      end

      def render_review_markdown(verdicts)
        lines = ["# Review Report", ""]
        lines << "_#{Time.now.strftime('%Y-%m-%d %H:%M')}_"
        lines << ""

        passed  = verdicts.select { |v| v[:verdict] == :passed }
        flagged = verdicts.select { |v| v[:verdict] == :flagged }
        skipped = verdicts.select { |v| v[:verdict] == :skipped }

        lines << "**#{passed.size}** passed · **#{flagged.size}** flagged · **#{skipped.size}** skipped"
        lines << ""

        if flagged.any?
          lines << "## Flagged"
          lines << ""
          flagged.each do |v|
            lines << "- **#{v[:name]}**"
            lines << "  - #{v[:note]}" if v[:note] && !v[:note].empty?
          end
          lines << ""
        end

        if passed.any?
          lines << "## Passed"
          lines << ""
          passed.each { |v| lines << "- #{v[:name]}" }
          lines << ""
        end

        if skipped.any?
          lines << "## Skipped"
          lines << ""
          skipped.each { |v| lines << "- #{v[:name]}" }
          lines << ""
        end

        lines.join("\n")
      end

      def run_batch(to_run)
        puts "Running #{to_run.size} scenario(s)...\n\n"
        passed = 0
        failed = 0

        to_run.each do |s|
          print "  #{s.name} ... "
          begin
            s.run(@binary, profile_dir: @profile_dir)
            cache_diff(s.name.to_s, s.last_diff)
            if s.failures.empty?
              puts "\e[32m✓\e[0m"
              passed += 1
            else
              puts "\e[31m✗\e[0m"
              puts "    \e[2m$ #{s.last_cmd}\e[0m" if s.last_cmd
              puts "    #{s.onboarded? ? "\e[32m● onboarded\e[0m" : "\e[33m○ not onboarded\e[0m"}"
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
