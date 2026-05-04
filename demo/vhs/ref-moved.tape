Output ref-moved.gif

Set Shell "zsh"
Set FontFamily "JetBrainsMono Nerd Font"
Set FontSize 14
Set Width 1200
Set Height 600
Set Framerate 24
Set PlaybackSpeed 1.5
Set TypingSpeed 50ms
Set Theme "GitHub Dark"

Type "echo '# Tag moved forward — routine upstream release (REF_MOVED is a warning, not an error)'"
Enter
Sleep 1s

Type "grep -E 'uses:|  -' demo/workflows-pwned/5-pinned-before-update.yml"
Enter
Sleep 2s

Type "echo '# Check detects the tag now points to a newer commit — warning only'"
Enter
Sleep 1s

Type "gh actions-pin check --no-interactive demo/workflows-pwned/5-pinned-before-update.yml"
Enter
Sleep 10s

Sleep 3s
