Output check-autofix.gif

Set Shell "zsh"
Set FontFamily "JetBrainsMono Nerd Font"
Set FontSize 14
Set Width 1200
Set Height 700
Set Framerate 24
Set PlaybackSpeed 1.5
Set TypingSpeed 50ms
Set Theme "GitHub Dark"

Type "echo '# Unpinned workflow — 4 actions using mutable tags, including a nested composite'"
Enter
Sleep 1s

Type "grep uses: demo/workflows-check/ci.yml"
Enter
Sleep 2s

Type "echo '# Pin all actions (non-interactive auto-fix)'"
Enter
Sleep 1s

Type "gh actions-pin check --no-interactive demo/workflows-check/ci.yml"
Enter
Sleep 12s

Type "echo '# Lockfile written — check the result'"
Enter
Sleep 1s

Type "awk '/^# Automatically generated/ || /^dependencies:/ || /^  # / || /^  - / { print }' demo/workflows-check/ci.yml"
Enter
Sleep 3s
