# Go Sound

Go Sound is a simple application built in Go that monitors a Windows sound device for audio playback. If audio playback is recognized, a MQTT message is being sent. If no audio playback has been recognized for a configurable amount of time another MQTT message is being sent.

For convinience, the application has no window and runs in the Windows systray. Based on the icon you can quickly see the current status and toggle it manually.

## Intention

Integrate with [SENERGY Platform](https://github.com/senergy-platform) to automate something based on audio playback.
