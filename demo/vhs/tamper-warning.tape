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

Type "echo '# Tag moved BACKWARD to an older commit — same-repo rollback'"
Enter
Sleep 1s

Type "grep -E 'uses:|  -' demo/workflows-pwned/2-pinned-before-lineage-rewrite.yml"
Enter
Sleep 2s

Type "echo '# REF_MOVED warning fires — commit is real (on a branch), not a fork injection'"
Enter
Sleep 1s

Type "gh actions-pin check --no-interactive demo/workflows-pwned/2-pinned-before-lineage-rewrite.yml"
Enter
Sleep 10s

Sleep 3s
