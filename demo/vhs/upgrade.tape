Output upgrade.gif

Set Shell "zsh"
Set FontFamily "JetBrainsMono Nerd Font"
Set FontSize 14
Set Width 1200
Set Height 700
Set Framerate 24
Set PlaybackSpeed 1.5
Set TypingSpeed 50ms
Set Theme "GitHub Dark"

Type "echo '# Currently pinned to older versions'"
Enter
Sleep 1s

Type "grep -E 'uses:|  -' demo/workflows-upgrade/ci.yml"
Enter
Sleep 2s

Type "echo '# Upgrade all actions to latest'"
Enter
Sleep 1s

Type "gh actions-pin upgrade demo/workflows-upgrade/ci.yml"
Enter
Sleep 10s

# Select all upgrades
Type "a"
Sleep 500ms
Enter
Sleep 3s

# Confirm
Type "y"
Enter
Sleep 5s

Type "echo '# Updated refs and lockfile'"
Enter
Sleep 1s

Type "grep -E 'uses:|  -' demo/workflows-upgrade/ci.yml"
Enter
Sleep 3s
