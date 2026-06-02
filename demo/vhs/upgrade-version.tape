Output demo/vhs/out/upgrade-version.gif

Set Shell "zsh"
Set FontFamily "JetBrainsMono Nerd Font"
Set FontSize 14
Set Width 1200
Set Height 700
Set Framerate 24
Set PlaybackSpeed 1.5
Set TypingSpeed 50ms
Set Theme "GitHub Dark"

Type "echo '# Currently pinned to actions/checkout@v4.2.0'"
Enter
Sleep 1s

Type "grep -E 'uses:|  -' demo/workflows-upgrade/ci.yml | grep -i checkout"
Enter
Sleep 2s

Type "echo '# Upgrade to a specific version (v5), not latest'"
Enter
Sleep 1s

Type "gh actions-pin upgrade --action actions/checkout --version v5 demo/workflows-upgrade/ci.yml"
Enter
Sleep 12s

Type "echo '# Verify the pinned version'"
Enter
Sleep 1s

Type "grep -E 'uses:|  -' demo/workflows-upgrade/ci.yml | grep -i checkout"
Enter
Sleep 3s
