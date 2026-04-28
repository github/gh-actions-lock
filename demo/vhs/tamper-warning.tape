Output tamper-warning.gif

Set Shell "zsh"
Set FontFamily "JetBrainsMono Nerd Font"
Set FontSize 14
Set Width 1200
Set Height 700
Set Framerate 24
Set PlaybackSpeed 1.5
Set TypingSpeed 5ms

# Scene: Tag hijacked — tamper + unreachable warnings with guidance
Type "# Checking a workflow where the tag was hijacked to a malicious commit"
Enter
Sleep 500ms
Type "gh actions-pin check demo/workflows-pwned/1-pinned-before-hijack.yml"
Enter
Sleep 10s
