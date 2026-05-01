Output tamper-warning.gif

Set Shell "zsh"
Set FontFamily "JetBrainsMono Nerd Font"
Set FontSize 14
Set Width 1200
Set Height 700
Set Framerate 24
Set PlaybackSpeed 1.5
Set TypingSpeed 50ms
Set Theme "GitHub Dark"

Type "echo '# Workflow was pinned to a legitimate SHA'"
Enter
Sleep 1s

Type "grep -E 'uses:|  -' demo/workflows-pwned/1-pinned-before-hijack.yml"
Enter
Sleep 2s

Type "echo '# Attacker moved the tag to a fork-network commit'"
Enter
Sleep 1s

Type "gh actions-pin check --no-interactive demo/workflows-pwned/1-pinned-before-hijack.yml"
Enter
Sleep 10s

Sleep 3s
