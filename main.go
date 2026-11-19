package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/getlantern/systray"
	"github.com/go-ole/go-ole"
	"github.com/moutend/go-wca/pkg/wca"
)

type Config struct {
	DeviceName string `json:"device_name"`
	DeviceType string `json:"device_type"`
	MQTT       struct {
		Broker   string `json:"broker"`
		Port     int    `json:"port"`
		Username string `json:"username"`
		Password string `json:"password"`
		ClientID string `json:"client_id"`
	} `json:"mqtt"`
	SoundTimeout string `json:"sound_timeout"`
}

var (
	lastSoundTime time.Time
	peakValue     float32
	statusItem    *systray.MenuItem
	cfg           *Config
	localId       string
	active        atomic.Bool
)

//go:embed red.ico
var iconRed []byte

//go:embed grey.ico
var iconGrey []byte

//go:embed green.ico
var iconGreen []byte

func main() {
	exePath, err := os.Executable()
	if err != nil {
		log.Fatal(err)
	}
	dir := filepath.Dir(exePath)
	configPath := filepath.Join(dir, "config.json")

	cfg, err = loadConfig(configPath)
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}
	localId = strings.ReplaceAll(strings.ToLower(cfg.DeviceName), " ", "_")

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	go func() {
		for range ticker.C {
			updateStatus()
		}
	}()

	go setupMqtt()
	startPowerWatcher()
	startWebcamWatcher()

	// Start systray
	systray.Run(onReady, onExit)
}

func setupMqtt() {
	// Initialize MQTT client
	opts := mqtt.NewClientOptions()
	brokerURL := fmt.Sprintf("tcp://%s:%d", cfg.MQTT.Broker, cfg.MQTT.Port)
	opts.AddBroker(brokerURL)
	opts.SetClientID(cfg.MQTT.ClientID)
	opts.SetUsername(cfg.MQTT.Username)
	opts.SetPassword(cfg.MQTT.Password)
	opts.SetAutoReconnect(true)
	opts.SetKeepAlive(15 * time.Second)
	opts.SetCleanSession(true)

	// Set last will testament on the lw topic of the device-manager, so that a lost connection
	// (standby, crash, network) marks the device offline, as onExit does on a clean exit. MQTT allows
	// one will only: a playing state left over from before the loss is corrected by the OnConnect
	// handler, which publishes the current value on every (re)connect.
	opts.SetWill("device-manager/device/"+cfg.MQTT.ClientID+"/lw", "offline", 1, false)

	// Set connection handler for birth message
	opts.SetOnConnectHandler(func(client mqtt.Client) {
		deadline := time.Now().Add(5 * time.Second)
		publishBirth(client, deadline)
		// Republish the current values so a reconnect cannot leave a stale one.
		publishPlaying(client, active.Load(), deadline)
		publishWebcam(client, deadline)
	})

	client := mqtt.NewClient(opts)
	setClient(client)
	var token mqtt.Token

	for token == nil || token.Error() != nil {
		token = client.Connect()
		token.Wait()
		if token.Error() != nil {
			log.Fatalf("Failed to connect to MQTT broker: %v", token.Error())
			time.Sleep(5 * time.Second)
		}
	}
}

func onReady() {
	systray.SetIcon(iconRed)
	systray.SetTitle("Sound Monitor")
	statusItem = systray.AddMenuItem("Status: Monitoring...", "Current status")
	statusItem.Disable() // Make it read-only

	// Update status every second
	go updateStatus()

	go func() {
		if err := monitorAudio(cfg.DeviceName); err != nil {
			log.Println("monitor error:", err)
		}
	}()

	mToggle := systray.AddMenuItem("Toggle", "Toggle state")
	go func() {
		for {
			<-mToggle.ClickedCh
			if active.Load() {
				onSoundStop()
			} else {
				onSoundStart()
			}
			updateStatus()
		}
	}()

	systray.AddSeparator()
	mQuit := systray.AddMenuItem("Quit", "Quit the app")
	go func() {
		<-mQuit.ClickedCh
		systray.Quit()
	}()

}

func updateStatus() {
	// Set debug info
	timeSinceSound := time.Since(lastSoundTime)
	minutes := int(timeSinceSound.Minutes())
	seconds := int(timeSinceSound.Seconds()) % 60
	statusText := fmt.Sprintf("Peak: %.4f | Silent: %dm %ds", peakValue, minutes, seconds)
	statusItem.SetTitle(statusText)

	// Set icon
	if c := getClient(); c == nil || c.IsConnected() == false {
		systray.SetIcon(iconRed)
	} else if active.Load() {
		systray.SetIcon(iconGreen)
	} else {
		systray.SetIcon(iconGrey)
	}
}

func onExit() {
	client := getClient()
	client.Publish("device-manager/device/"+cfg.MQTT.ClientID+"/lw", 1, false, "offline").Wait()
	client.Disconnect(250)
	log.Println("Exiting...")
}

func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// ---- AUDIO MONITOR ----

func monitorAudio(targetName string) error {
	var deviceEnumerator *wca.IMMDeviceEnumerator
	if err := ole.CoInitializeEx(0, ole.COINIT_MULTITHREADED); err != nil {
		return err
	}
	defer ole.CoUninitialize()

	if err := wca.CoCreateInstance(wca.CLSID_MMDeviceEnumerator, 0, wca.CLSCTX_ALL, wca.IID_IMMDeviceEnumerator, &deviceEnumerator); err != nil {
		return err
	}

	var collection *wca.IMMDeviceCollection
	if err := deviceEnumerator.EnumAudioEndpoints(wca.ERender, wca.DEVICE_STATE_ACTIVE, &collection); err != nil {
		return err
	}

	var count uint32
	if err := collection.GetCount(&count); err != nil {
		return err
	}

	var device *wca.IMMDevice
	for i := uint32(0); i < count; i++ {
		if err := collection.Item(i, &device); err != nil {
			continue
		}
		var propStore *wca.IPropertyStore
		if err := device.OpenPropertyStore(wca.STGM_READ, &propStore); err != nil {
			continue
		}
		var pv wca.PROPVARIANT
		if err := propStore.GetValue(&wca.PKEY_Device_FriendlyName, &pv); err == nil {
			name := pv.String()
			if name == targetName {
				fmt.Println("Monitoring device:", name)
				monitorDevice(device)
				return nil
			}
		}
	}

	return fmt.Errorf("device not found: %s", targetName)
}

func monitorDevice(device *wca.IMMDevice) {
	var audioMeter *wca.IAudioMeterInformation
	if err := device.Activate(wca.IID_IAudioMeterInformation, wca.CLSCTX_ALL, nil, &audioMeter); err != nil {
		log.Println("Failed to activate device:", err)
		return
	}

	lastSoundTime = time.Now()
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	var peak float32
	calledOn := false
	calledOff := false
	soundTimeout, err := time.ParseDuration(cfg.SoundTimeout)
	if err != nil {
		log.Println("Failed to parse sound timeout:", err)
		soundTimeout = 5 * time.Minute // Default to 5 minutes if parsing fails
	}

	for range ticker.C {
		if err := audioMeter.GetPeakValue(&peak); err != nil {
			log.Println("GetPeakValue:", err)
			continue
		}

		peakValue = peak // Update the global peakValue

		if peak > 0.001 {
			lastSoundTime = time.Now()
			if !calledOn {
				ok := onSoundStart()
				if !ok {
					continue
				}
				calledOn = true
				calledOff = false
			}
		} else if time.Since(lastSoundTime) > silenceTimeout(soundTimeout, pres.isLocked()) && !calledOff {
			ok := onSoundStop()
			if !ok {
				continue
			}
			calledOn = false
			calledOff = true
		}
	}
}

// ---- ACTIONS ----

func onSoundStart() bool {
	client := getClient()
	if client == nil {
		log.Println("MQTT client is not initialized")
		return false
	}
	active.Store(true)
	t, _ := enqueuePlaying(client, true, true)
	if t == nil {
		// Suspended: resume publishes the current value.
		log.Println("Sound detected while suspended, publish deferred")
		return true
	}
	go func() {
		t.Wait()
		if t.Error() != nil {
			log.Println("Failed to publish MQTT message for sound detection")
		} else {
			log.Println("Published MQTT message for sound detection")
		}
	}()
	return true
}

func onSoundStop() bool {
	client := getClient()
	if client == nil {
		log.Println("MQTT client is not initialized")
		return false
	}
	active.Store(false)
	t, _ := enqueuePlaying(client, false, true)
	if t == nil {
		log.Println("Silence detected while suspended, publish deferred")
		return true
	}
	go func() {
		t.Wait()
		if t.Error() != nil {
			log.Println("Failed to publish MQTT message for no sound")
		} else {
			log.Println("Published MQTT message for no sound")
		}
	}()
	return true
}
