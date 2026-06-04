Output demo/vhs/out/parallel-pin.gif

Set Shell "zsh"
Set FontFamily "JetBrainsMono Nerd Font"
Set FontSize 13
Set Width 1400
Set Height 800
Set Framerate 24
Set PlaybackSpeed 1.0
Set TypingSpeed 40ms
Set Theme "GitHub Dark"

Env REPO_ROOT "/Users/nodeselector/ghq/github.com/github/gh-actions-pin"

# Land in a clean throwaway repo seeded from demo/workflows so the recording
# has 5+ workflow files for the parallel pinner to chew on.
Hide
Type "rm -rf /tmp/parallel-pin && mkdir -p /tmp/parallel-pin/.github/workflows && cd /tmp/parallel-pin && git init -q && cp $REPO_ROOT/demo/workflows/*.yml .github/workflows/ && clear"
Enter
Sleep 500ms
Show

Type "ls .github/workflows/"
Enter
Sleep 2s

Type "echo '# Pin everything in parallel (8-worker pool)'"
Enter
Sleep 1s

Type "gh actions-pin --no-interactive"
Enter
Sleep 30s
