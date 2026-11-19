// Copyright (c) 2026 InfAI (CC SES)

//go:build windows

package main

import (
	"fmt"
	"log"
	"runtime"
	"sync/atomic"
	"syscall"
	"unsafe"

	"github.com/go-ole/go-ole"
	"golang.org/x/sys/windows"
)

// The camera state comes from the Media Foundation sensor activity monitor
// (Windows 10 1703 or later): Windows calls OnActivitiesReport with the
// processes that use a camera and whether they stream. The webcam consent
// store in the registry is not used: Windows stopped updating it on this
// machine. IIDs and vtable layout as in mfidl.h.

var (
	iidIUnknown                          = ole.IID_IUnknown
	iidAgileObject                       = ole.NewGUID("{94EA2B94-E9CC-49E0-C0FF-EE64CA8F5B90}")
	iidIMFSensorActivitiesReportCallback = ole.NewGUID("{DE5072EE-DBE3-46DC-8A87-B6F631194751}")

	procMFStartup                     = windows.NewLazySystemDLL("mfplat.dll").NewProc("MFStartup")
	procMFCreateSensorActivityMonitor = windows.NewLazySystemDLL("mfsensorgroup.dll").NewProc("MFCreateSensorActivityMonitor")
)

const (
	mfVersion       = 0x00020070 // MF_SDK_VERSION << 16 | MF_API_VERSION
	mfStartupFull   = 0
	hrOK            = 0
	hrNoInterface   = 0x80004002
	hrPointer       = 0x80004003
	ptrSize         = unsafe.Sizeof(uintptr(0))
	slotRelease     = 2
	slotReportCount = 3 // IMFSensorActivitiesReport::GetCount
	slotReportAt    = 4 // IMFSensorActivitiesReport::GetActivityReport
	slotProcCount   = 5 // IMFSensorActivityReport::GetProcessCount
	slotProcAt      = 6 // IMFSensorActivityReport::GetProcessActivity
	slotStreaming   = 4 // IMFSensorProcessActivity::GetStreamingState
	slotStart       = 3 // IMFSensorActivityMonitor::Start
)

// sensorCallback is a static COM object implementing
// IMFSensorActivitiesReportCallback. It lives for the whole process, so
// AddRef and Release only count.
type sensorCallback struct {
	vtbl *sensorCallbackVtbl
	refs int32
}

type sensorCallbackVtbl struct {
	QueryInterface     uintptr
	AddRef             uintptr
	Release            uintptr
	OnActivitiesReport uintptr
}

var (
	// filled in startSensorMonitor: the callbacks refer to callbackObj, so a
	// package-level initialiser would be a cycle
	callbackVtbl sensorCallbackVtbl
	callbackObj  sensorCallback

	monitor uintptr // IMFSensorActivityMonitor, kept for the life of the process
)

func startWebcamWatcher() {
	go runWebcamPublisher()
	ready := make(chan error, 1)
	go func() {
		// COM and Media Foundation stay initialised on this thread for the
		// life of the process; the callbacks arrive on Media Foundation threads.
		runtime.LockOSThread()
		ready <- startSensorMonitor()
		select {}
	}()
	if err := <-ready; err != nil {
		log.Println("Webcam: detection unavailable:", err)
		return
	}
	log.Println("Webcam: sensor activity monitor started")
}

func startSensorMonitor() error {
	if err := ole.CoInitializeEx(0, ole.COINIT_MULTITHREADED); err != nil {
		// S_FALSE: already initialised on this thread, which is fine
		if oe, ok := err.(*ole.OleError); !ok || oe.Code() != 1 {
			return fmt.Errorf("CoInitializeEx: %w", err)
		}
	}
	if err := procMFStartup.Find(); err != nil {
		return err
	}
	if hr, _, _ := procMFStartup.Call(mfVersion, mfStartupFull); hr != hrOK {
		return fmt.Errorf("MFStartup: HRESULT 0x%08x", uint32(hr))
	}
	if err := procMFCreateSensorActivityMonitor.Find(); err != nil {
		return err
	}
	callbackVtbl = sensorCallbackVtbl{
		QueryInterface:     windows.NewCallback(cbQueryInterface),
		AddRef:             windows.NewCallback(cbAddRef),
		Release:            windows.NewCallback(cbRelease),
		OnActivitiesReport: windows.NewCallback(cbOnActivitiesReport),
	}
	callbackObj = sensorCallback{vtbl: &callbackVtbl, refs: 1}
	hr, _, _ := procMFCreateSensorActivityMonitor.Call(uintptr(unsafe.Pointer(&callbackObj)), uintptr(unsafe.Pointer(&monitor)))
	if hr != hrOK || monitor == 0 {
		return fmt.Errorf("MFCreateSensorActivityMonitor: HRESULT 0x%08x", uint32(hr))
	}
	if hr := comCall(monitor, slotStart); hr != hrOK {
		return fmt.Errorf("IMFSensorActivityMonitor::Start: HRESULT 0x%08x", uint32(hr))
	}
	return nil
}

// toPtr turns an address handed over by Windows into a pointer.
func toPtr(u uintptr) unsafe.Pointer {
	return *(*unsafe.Pointer)(unsafe.Pointer(&u))
}

// comCall calls the method in slot of the vtable of obj.
func comCall(obj uintptr, slot uintptr, args ...uintptr) uintptr {
	vtbl := *(*uintptr)(toPtr(obj))
	fn := *(*uintptr)(toPtr(vtbl + slot*ptrSize))
	hr, _, _ := syscall.SyscallN(fn, append([]uintptr{obj}, args...)...)
	return hr
}

func comRelease(obj uintptr) {
	if obj != 0 {
		comCall(obj, slotRelease)
	}
}

func cbQueryInterface(this, riid, ppv uintptr) uintptr {
	if ppv == 0 {
		return hrPointer
	}
	out := (*uintptr)(toPtr(ppv))
	iid := (*ole.GUID)(toPtr(riid))
	if ole.IsEqualGUID(iid, iidIUnknown) || ole.IsEqualGUID(iid, iidIMFSensorActivitiesReportCallback) || ole.IsEqualGUID(iid, iidAgileObject) {
		*out = this
		atomic.AddInt32(&callbackObj.refs, 1)
		return hrOK
	}
	*out = 0
	return hrNoInterface
}

func cbAddRef(this uintptr) uintptr {
	return uintptr(atomic.AddInt32(&callbackObj.refs, 1))
}

func cbRelease(this uintptr) uintptr {
	return uintptr(atomic.AddInt32(&callbackObj.refs, -1))
}

// cbOnActivitiesReport takes the state from the report. A report without
// sensors means no camera is in use. A report that cannot be read leaves
// the state as it is.
func cbOnActivitiesReport(this, report uintptr) uintptr {
	if report == 0 {
		return hrOK
	}
	streaming, err := anyStreaming(report)
	if err != nil {
		log.Println("Webcam: report not readable:", err)
		return hrOK
	}
	setWebcam(streaming)
	return hrOK
}

func anyStreaming(report uintptr) (bool, error) {
	var count uint32
	if hr := comCall(report, slotReportCount, uintptr(unsafe.Pointer(&count))); hr != hrOK {
		return false, fmt.Errorf("GetCount: HRESULT 0x%08x", uint32(hr))
	}
	for i := uint32(0); i < count; i++ {
		var sensor uintptr
		if hr := comCall(report, slotReportAt, uintptr(i), uintptr(unsafe.Pointer(&sensor))); hr != hrOK {
			return false, fmt.Errorf("GetActivityReport: HRESULT 0x%08x", uint32(hr))
		}
		streaming, err := sensorStreaming(sensor)
		comRelease(sensor)
		if err != nil {
			return false, err
		}
		if streaming {
			return true, nil
		}
	}
	return false, nil
}

func sensorStreaming(sensor uintptr) (bool, error) {
	var procs uint32
	if hr := comCall(sensor, slotProcCount, uintptr(unsafe.Pointer(&procs))); hr != hrOK {
		return false, fmt.Errorf("GetProcessCount: HRESULT 0x%08x", uint32(hr))
	}
	for j := uint32(0); j < procs; j++ {
		var proc uintptr
		if hr := comCall(sensor, slotProcAt, uintptr(j), uintptr(unsafe.Pointer(&proc))); hr != hrOK {
			return false, fmt.Errorf("GetProcessActivity: HRESULT 0x%08x", uint32(hr))
		}
		var state int32 // BOOL
		hr := comCall(proc, slotStreaming, uintptr(unsafe.Pointer(&state)))
		comRelease(proc)
		if hr != hrOK {
			return false, fmt.Errorf("GetStreamingState: HRESULT 0x%08x", uint32(hr))
		}
		if state != 0 {
			return true, nil
		}
	}
	return false, nil
}
