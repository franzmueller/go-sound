// Copyright (c) 2026 InfAI (CC SES)

//go:build windows

package main

import (
	"encoding/binary"
	"log"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	wmPowerBroadcast   = 0x0218
	wmWtsSessionChange = 0x02B1

	pbtAPMSuspend         = 0x4
	pbtAPMResumeSuspend   = 0x7
	pbtAPMResumeAutomatic = 0x12
	pbtPowerSettingChange = 0x8013

	wtsSessionLock   = 0x7
	wtsSessionUnlock = 0x8

	notifyForThisSession    = 0
	deviceNotifyWindowHndl  = 0           // DEVICE_NOTIFY_WINDOW_HANDLE
	hwndMessage             = ^uintptr(2) // HWND_MESSAGE (-3)
	powerWatcherClassName   = "GoSoundPowerWatcher"
	powerWatcherWindowTitle = "GoSoundPowerWatcher"
)

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	wtsapi32 = windows.NewLazySystemDLL("wtsapi32.dll")

	procGetModuleHandleW               = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetModuleHandleW")
	procRegisterClassExW               = user32.NewProc("RegisterClassExW")
	procCreateWindowExW                = user32.NewProc("CreateWindowExW")
	procDefWindowProcW                 = user32.NewProc("DefWindowProcW")
	procGetMessageW                    = user32.NewProc("GetMessageW")
	procTranslateMessage               = user32.NewProc("TranslateMessage")
	procDispatchMessageW               = user32.NewProc("DispatchMessageW")
	procRegisterSuspendResumeNotify    = user32.NewProc("RegisterSuspendResumeNotification")
	procRegisterPowerSettingNotify     = user32.NewProc("RegisterPowerSettingNotification")
	procWTSRegisterSessionNotification = wtsapi32.NewProc("WTSRegisterSessionNotification")
	guidSessionDisplayStatus           = windows.GUID{Data1: 0x2B84C20E, Data2: 0xAD23, Data3: 0x4DDF, Data4: [8]byte{0x93, 0xDB, 0x05, 0xFF, 0xBD, 0x7E, 0xFC, 0xA5}}
)

type wndClassEx struct {
	cbSize        uint32
	style         uint32
	lpfnWndProc   uintptr
	cbClsExtra    int32
	cbWndExtra    int32
	hInstance     windows.Handle
	hIcon         windows.Handle
	hCursor       windows.Handle
	hbrBackground windows.Handle
	lpszMenuName  *uint16
	lpszClassName *uint16
	hIconSm       windows.Handle
}

type winMsg struct {
	hwnd    windows.Handle
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	ptX     int32
	ptY     int32
}

// startPowerWatcher runs a hidden message-only window on its own OS thread.
// It does not touch the systray message loop.
func startPowerWatcher() {
	go func() {
		runtime.LockOSThread()
		if err := runPowerWatcher(); err != nil {
			log.Println("power watcher stopped:", err)
		}
	}()
}

func runPowerWatcher() error {
	hInstR, _, e := procGetModuleHandleW.Call(0)
	if hInstR == 0 {
		return e
	}
	hInst := windows.Handle(hInstR)
	className, err := windows.UTF16PtrFromString(powerWatcherClassName)
	if err != nil {
		return err
	}
	title, err := windows.UTF16PtrFromString(powerWatcherWindowTitle)
	if err != nil {
		return err
	}

	wndProc := windows.NewCallback(powerWndProc)
	wc := wndClassEx{
		lpfnWndProc:   wndProc,
		hInstance:     hInst,
		lpszClassName: className,
	}
	wc.cbSize = uint32(unsafe.Sizeof(wc))
	if r, _, e := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
		return e
	}

	hwnd, _, e := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(title)),
		0,
		0, 0, 0, 0,
		hwndMessage,
		0,
		uintptr(hInst),
		0,
	)
	if hwnd == 0 {
		return e
	}

	// A message-only window gets no broadcast messages, so the suspend/resume
	// events must be requested explicitly. Per Microsoft this also delivers
	// the notification on connected / Modern Standby (Desktop Activity
	// Moderator), not only on S3.
	if h, _, e := procRegisterSuspendResumeNotify.Call(hwnd, deviceNotifyWindowHndl); h == 0 {
		log.Println("RegisterSuspendResumeNotification failed, standby will not be reported:", e)
	} else {
		log.Println("Registered for suspend/resume notifications")
	}

	if r, _, e := procWTSRegisterSessionNotification.Call(hwnd, notifyForThisSession); r == 0 {
		log.Println("WTSRegisterSessionNotification failed, lock/unlock will not be reported:", e)
	} else {
		log.Println("Registered for session lock/unlock notifications")
	}

	// Diagnostics only: log display on/off. Not used to decide the state.
	if h, _, e := procRegisterPowerSettingNotify.Call(hwnd, uintptr(unsafe.Pointer(&guidSessionDisplayStatus)), deviceNotifyWindowHndl); h == 0 {
		log.Println("RegisterPowerSettingNotification (display) failed:", e)
	}

	var m winMsg
	for {
		r, _, e := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		switch int32(r) {
		case 0: // WM_QUIT
			return nil
		case -1:
			return e
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
}

func powerWndProc(hwnd, msg, wParam, lParam uintptr) uintptr {
	switch msg {
	case wmPowerBroadcast:
		switch wParam {
		case pbtAPMSuspend:
			onSystemSuspend()
		case pbtAPMResumeSuspend, pbtAPMResumeAutomatic:
			onSystemResume()
		case pbtPowerSettingChange:
			logDisplayState(lParam)
		}
		return 1 // TRUE: processed
	case wmWtsSessionChange:
		switch wParam {
		case wtsSessionLock:
			onSessionLock()
		case wtsSessionUnlock:
			onSessionUnlock()
		}
		return 0
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, msg, wParam, lParam)
	return r
}

// logDisplayState reads a POWERBROADCAST_SETTING: GUID (16 bytes),
// DataLength (4 bytes), then Data.
func logDisplayState(lParam uintptr) {
	if lParam == 0 {
		return
	}
	p := *(*unsafe.Pointer)(unsafe.Pointer(&lParam))
	head := unsafe.Slice((*byte)(p), 20)
	if *(*windows.GUID)(p) != guidSessionDisplayStatus {
		return
	}
	if binary.LittleEndian.Uint32(head[16:20]) < 4 {
		return
	}
	data := unsafe.Slice((*byte)(unsafe.Add(p, 20)), 4)
	switch binary.LittleEndian.Uint32(data) {
	case 0:
		log.Println("Display: off")
	case 1:
		log.Println("Display: on")
	case 2:
		log.Println("Display: dimmed")
	}
}
