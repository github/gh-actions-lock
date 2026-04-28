Output sha-as-ref.gif

Set Shell "zsh"
Set FontFamily "JetBrainsMono Nerd Font"
Set FontSize 14
Set Width 1200
Set Height 700
Set Framerate 24
Set PlaybackSpeed 1.5
Set TypingSpeed 5ms

# Scene: Action uses a bare SHA — tool detects and presents tag picker
Type "# Action uses a bare SHA instead of a tag ref"
Enter
Sleep 500ms
Type "grep 'uses:' demo/workflows-interactive/sha-as-ref.yml"
Enter
Sleep 1s

Type "gh actions-pin check demo/workflows-interactive/sha-as-ref.yml"
Enter
Sleep 8s

# Tag picker is visible — select v1.0.1 (already highlighted as recommended)
Enter
Sleep 3s

# Confirm pin
Type "y"
Enter
Sleep 5s
