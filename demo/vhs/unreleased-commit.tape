Output demo/vhs/out/unreleased-commit.gif

Set Shell "zsh"
Set FontFamily "JetBrainsMono Nerd Font"
Set FontSize 14
Set Width 1200
Set Height 700
Set Framerate 24
Set PlaybackSpeed 1.5
Set TypingSpeed 50ms
Set Theme "GitHub Dark"

Type "echo '# Action pinned to a commit with no tagged release'"
Enter
Sleep 1s

Type "grep uses: demo/workflows-interactive/unreleased-commit.yml"
Enter
Sleep 2s

Type "echo '# The tool warns and offers a tag picker'"
Enter
Sleep 1s

Type "gh actions-pin check demo/workflows-interactive/unreleased-commit.yml"
Enter
Sleep 8s

# Select first tag option
Enter
Sleep 3s

# Confirm
Type "y"
Enter
Sleep 5s
