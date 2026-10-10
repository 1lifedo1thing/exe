//go:build darwin

// Package menubar puts a status item in the macOS menu bar while the daemon
// runs: open the web UI, restart the daemon, or quit it (with confirmation)
// without hunting for the terminal it was started from.
package menubar

// Info.plist is linked into the binary (the __info_plist section, the
// one place macOS looks for a property list in a program that is not an
// app bundle). exe is a single file with a menu-bar item and a launchd
// agent, and macOS wants to know such a program by more than its path:
// the identifier names it in Login Items and in Privacy & Security, and
// NSLocalNetworkUsageDescription is the sentence in the alert that asks
// to let it reach the local network — which is where its VMs are, and
// which a launchd agent, unlike a tool typed into Terminal, has to be
// granted (Apple's TN3179). The identifier is the one the release is
// signed with and the launchd agent is named for.

/*
#cgo CFLAGS: -x objective-c -fobjc-arc
#cgo LDFLAGS: -framework Cocoa -framework CoreGraphics -Wl,-sectcreate,__TEXT,__info_plist,${SRCDIR}/Info.plist

#include <stdlib.h>

int menubarSupported(void);
void menubarRun(void);
*/
import "C"

import "runtime"

func init() {
	// AppKit only runs its event loop on the process's first thread. Package
	// inits run on the main goroutine before main(), so pin it here.
	runtime.LockOSThread()
}

// Handlers are the menu actions. They run on the AppKit main thread, so
// anything slow must hop to a goroutine.
type Handlers struct {
	OpenUI      func()
	Restart     func()
	QuitMessage func() string // body of the quit confirmation dialog
	Quit        func()        // runs only after the user confirms
}

var handlers Handlers

// Supported reports whether this process can reach the window server; it is
// false under ssh and other headless sessions, where the daemon must run
// without an icon.
func Supported() bool { return C.menubarSupported() != 0 }

// Run installs the status item and enters the AppKit event loop. It must be
// called on the main goroutine and never returns.
func Run(h Handlers) {
	handlers = h
	C.menubarRun()
}

//export goMenuOpenUI
func goMenuOpenUI() {
	if handlers.OpenUI != nil {
		handlers.OpenUI()
	}
}

//export goMenuRestart
func goMenuRestart() {
	if handlers.Restart != nil {
		handlers.Restart()
	}
}

//export goMenuQuitMessage
func goMenuQuitMessage() *C.char {
	msg := "This stops the exe daemon."
	if handlers.QuitMessage != nil {
		msg = handlers.QuitMessage()
	}
	return C.CString(msg) // freed by the ObjC caller
}

//export goMenuQuit
func goMenuQuit() {
	if handlers.Quit != nil {
		handlers.Quit()
	}
}
