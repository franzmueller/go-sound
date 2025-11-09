GOOS=windows go build -o sound-monitor-debug.exe
GOOS=windows go build -ldflags "-H=windowsgui" -o sound-monitor.exe