Output unreleased-commit.gif

Set Shell "zsh"
Set FontFamily "JetBrainsMono Nerd Font"
Set FontSize 14
Set Width 1200
Set Height 700
Set Framerate 24
Set PlaybackSpeed 1.5
Set TypingSpeed 5ms
Set Theme "GitHub Dark"

# Scene: Action pinned to an unreleased commit — warning + tag picker
Type "# Action pinned to a commit with no tagged release"
Enter
Sleep 500ms
Type "grep 'uses:' demo/workflows-interactive/unreleased-commit.yml"
Enter
Sleep 1s

Type "gh actions-pin check demo/workflows-interactive/unreleased-commit.yml"
Enter
Sleep 8s

# Tag picker visible — select v2.0.0 (first option)
Enter
Sleep 3s

# Confirm pin
Type "y"
Enter
Sleep 5s
