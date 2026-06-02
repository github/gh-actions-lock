Output demo/vhs/out/sha-as-ref.gif

Set Shell "zsh"
Set FontFamily "JetBrainsMono Nerd Font"
Set FontSize 14
Set Width 1200
Set Height 700
Set Framerate 24
Set PlaybackSpeed 1.5
Set TypingSpeed 50ms
Set Theme "GitHub Dark"

Type "echo '# Action uses a bare SHA instead of a tag'"
Enter
Sleep 1s

Type "grep uses: demo/workflows-interactive/sha-as-ref.yml"
Enter
Sleep 2s

Type "echo '# The tool detects this and offers tag suggestions'"
Enter
Sleep 1s

Type "gh actions-pin check demo/workflows-interactive/sha-as-ref.yml"
Enter
Sleep 8s

# Select first suggestion (recommended tag)
Enter
Sleep 3s

# Confirm
Type "y"
Enter
Sleep 5s
