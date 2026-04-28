Output check-autofix.gif

Set Shell zsh
Set FontFamily "JetBrainsMono Nerd Font"
Set FontSize 14
Set Width 1200
Set Height 700
Set Framerate 24
Set PlaybackSpeed 1.5
Set TypingSpeed 5ms
Set Theme "GitHub Dark"

# Show the unpinned workflow
Type "cat demo/workflows-check/ci.yml"
Enter 1
Sleep 2s

# Run check to detect unpinned actions and auto-fix
Type "gh actions-pin check --fix demo/workflows-check/ci.yml"
Enter 1
Sleep 12s

# Show the pinned result
Type "cat demo/workflows-check/ci.yml"
Enter 1
Sleep 3s
