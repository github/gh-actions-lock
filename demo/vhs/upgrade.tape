Output upgrade.gif

Set Shell zsh
Set FontFamily "JetBrainsMono Nerd Font"
Set FontSize 14
Set Width 1200
Set Height 700
Set Framerate 24
Set PlaybackSpeed 1.5
Set TypingSpeed 5ms
Set Theme "GitHub Dark"

# Show current pinned versions
Type "grep -A1 'uses:\|dependencies:' demo/workflows-upgrade/ci.yml | head -20"
Enter 1
Sleep 2s

# Run upgrade to find newer versions
Type "gh actions-pin upgrade demo/workflows-upgrade/ci.yml"
Enter 1
Sleep 10s

# Select all upgrades
Type "a"
Sleep 500ms
Enter 1
Sleep 3s

# Confirm
Type "y"
Enter 1
Sleep 5s

# Show updated versions
Type "grep 'uses:\|  -' demo/workflows-upgrade/ci.yml"
Enter 1
Sleep 3s
