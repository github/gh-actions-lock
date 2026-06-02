Output demo/vhs/out/edit-repin.gif

Set Shell "zsh"
Set FontFamily "JetBrainsMono Nerd Font"
Set FontSize 14
Set Width 1200
Set Height 700
Set Framerate 24
Set PlaybackSpeed 1.5
Set TypingSpeed 50ms
Set Theme "GitHub Dark"

Type "grep checkout demo/workflows-upgrade/ci.yml"
Enter
Sleep 2s

Type "echo '# Edit the ref'"
Enter
Sleep 1s

Type "sed -i '' 's/checkout@v4.2.0/checkout@v5/g' demo/workflows-upgrade/ci.yml"
Enter
Sleep 1s

Type "echo '# Remove stale lockfile so check can re-resolve'"
Enter
Sleep 1s

Type "rm -f .github/workflows/actions.lock"
Enter
Sleep 1s

Type "echo '# Re-pin with check'"
Enter
Sleep 1s

Type "gh actions-pin check --no-interactive demo/workflows-upgrade/ci.yml"
Enter
Sleep 12s

Type "echo '# New lockfile written'"
Enter
Sleep 1s

Type "cat .github/workflows/actions.lock"
Enter
Sleep 3s
