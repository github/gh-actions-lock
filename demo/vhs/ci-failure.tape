Output ci-failure.gif

Set Shell "zsh"
Set FontFamily "JetBrainsMono Nerd Font"
Set FontSize 14
Set Width 1200
Set Height 700
Set Framerate 24
Set PlaybackSpeed 1.5
Set TypingSpeed 50ms
Set Theme "GitHub Dark"

Type "echo '# CI mode: auto-fix deterministic issues, fail on ambiguous ones'"
Enter
Sleep 1s

Type "grep uses: demo/workflows-interactive/sha-as-ref.yml"
Enter
Sleep 2s

Type "echo '# One action has a tag (fixable), one is a bare SHA (needs human)'"
Enter
Sleep 1s

Type "gh actions-pin check --no-interactive demo/workflows-interactive/sha-as-ref.yml"
Enter
Sleep 12s

Type "echo \"# Exit code: $?\""
Enter
Sleep 2s
