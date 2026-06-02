Output demo/vhs/out/lockfile-forgery.gif

Set Shell "zsh"
Set FontFamily "JetBrainsMono Nerd Font"
Set FontSize 14
Set Width 1200
Set Height 700
Set Framerate 24
Set PlaybackSpeed 1.5
Set TypingSpeed 50ms
Set Theme "GitHub Dark"

Type "echo '# Lockfile was tampered — pinned SHA replaced with a fork commit'"
Enter
Sleep 1s

Type "grep -E 'uses:|  -|FORGED' demo/workflows-pwned/6-lockfile-forgery.yml"
Enter
Sleep 2s

Type "echo '# Compare API detects the forged SHA is not in the ref lineage'"
Enter
Sleep 1s

Type "gh actions-pin check --no-interactive demo/workflows-pwned/6-lockfile-forgery.yml"
Enter
Sleep 10s

Sleep 3s
