//go:build windows

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"exe/internal/vmm"
)

// helperState: the network helper is a Linux thing.
func helperState(string) (sum string, rooted bool) { return "", false }

// What the installer keeps in the user's own registry: the command that
// runs at sign-in, and the user's PATH.
const (
	runKey = `Software\Microsoft\Windows\CurrentVersion\Run`
	envKey = `Environment`
)

// runValue names the sign-in entry; a test uses one of its own.
var runValue = "exe"

func newWinHost() *winHost {
	w := &winHost{CanVM: runtime.GOARCH == "amd64"}
	w.Elevated = windows.GetCurrentProcessToken().IsElevated()
	// A program started by sshd, or by a service, runs in session 0, where
	// no desktop is; the user's own programs run in the session they
	// signed in to.
	var session uint32
	if windows.ProcessIdToSessionId(windows.GetCurrentProcessId(), &session) == nil {
		w.Session = session != 0
	}
	w.Drives = winDrives
	w.WHPX = vmm.WHPXAvailable
	w.QEMU = vmm.FindQEMU
	w.Login = func() string {
		k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE)
		if err != nil {
			return ""
		}
		defer k.Close()
		v, _, _ := k.GetStringValue(runValue)
		return v
	}
	w.SetLogin = func(cmd string) error {
		k, _, err := registry.CreateKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
		if err != nil {
			return err
		}
		defer k.Close()
		if cmd == "" {
			if err := k.DeleteValue(runValue); err != nil && !errors.Is(err, registry.ErrNotExist) {
				return err
			}
			return nil
		}
		return k.SetStringValue(runValue, cmd)
	}
	w.AddPath = func(dir string) (bool, error) { return editUserPath(dir, true) }
	w.DelPath = func(dir string) error { _, err := editUserPath(dir, false); return err }
	w.Start = func(argv []string) error {
		// A console of its own, hidden, and its own process group:
		// closing the terminal the installer ran in must not take the
		// daemon with it, and the daemon needs a console to be told of a
		// sign-out (winLogin in install.go). And out of whatever job
		// this process is in: a program started by Task Scheduler is in
		// the task's job. A job that does not let go refuses the flag,
		// and then the daemon is started inside it.
		flags := []uint32{
			windows.CREATE_NEW_CONSOLE | windows.CREATE_NEW_PROCESS_GROUP | windows.CREATE_BREAKAWAY_FROM_JOB,
			windows.CREATE_NEW_CONSOLE | windows.CREATE_NEW_PROCESS_GROUP,
		}
		var err error
		for _, f := range flags {
			c := exec.Command(argv[0], argv[1:]...)
			c.SysProcAttr = &syscall.SysProcAttr{CreationFlags: f, HideWindow: true}
			if err = c.Start(); err == nil {
				return c.Process.Release()
			}
		}
		return err
	}
	w.Kill = killByPath
	w.DeleteLater = func(path string) error {
		// cmd waits a few seconds (ping is the sleep every Windows has),
		// then removes the file and, if nothing else is in it, its folder
		script := `ping -n 4 127.0.0.1 >nul & del /f /q "` + path + `" & rmdir "` + filepath.Dir(path) + `"`
		c := exec.Command(os.Getenv("ComSpec"), "/c", script)
		c.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP, CmdLine: `/c "` + script + `"`}
		if err := c.Start(); err != nil {
			return err
		}
		return c.Process.Release()
	}
	return w
}

// editUserPath adds dir to, or takes it out of, the user's own PATH, and
// tells the running desktop so that the next terminal has it. changed says
// whether the PATH was touched.
func editUserPath(dir string, add bool) (changed bool, err error) {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, envKey, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return false, err
	}
	defer k.Close()
	cur, typ, err := k.GetStringValue("Path")
	if err != nil && !errors.Is(err, registry.ErrNotExist) {
		return false, err
	}
	var keep []string
	found := false
	for _, p := range strings.Split(cur, ";") {
		if strings.EqualFold(strings.TrimRight(p, `\`), strings.TrimRight(dir, `\`)) {
			found = true
			if !add {
				continue
			}
		}
		if p != "" {
			keep = append(keep, p)
		}
	}
	if add == found {
		return false, nil
	}
	if add {
		keep = append(keep, dir)
	}
	next := strings.Join(keep, ";")
	// a PATH that names %variables% is an expandable string, and stays one
	if typ == registry.EXPAND_SZ || strings.Contains(next, "%") {
		err = k.SetExpandStringValue("Path", next)
	} else {
		err = k.SetStringValue("Path", next)
	}
	if err != nil {
		return false, err
	}
	env, _ := windows.UTF16PtrFromString("Environment")
	const hwndBroadcast, wmSettingChange, abortIfHung = 0xffff, 0x001A, 0x0002
	windows.NewLazySystemDLL("user32.dll").NewProc("SendMessageTimeoutW").Call(
		hwndBroadcast, wmSettingChange, 0, uintptr(unsafe.Pointer(env)), abortIfHung, 2000, 0)
	return true, nil
}

// killByPath ends every process, other than this one, whose program is
// the file at bin, and says how many there were.
func killByPath(bin string) int {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return 0
	}
	defer windows.CloseHandle(snap)
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	n := 0
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		if e.ProcessID == windows.GetCurrentProcessId() || e.ProcessID == 0 {
			continue
		}
		h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_TERMINATE, false, e.ProcessID)
		if err != nil {
			continue
		}
		buf := make([]uint16, windows.MAX_LONG_PATH)
		size := uint32(len(buf))
		if windows.QueryFullProcessImageName(h, 0, &buf[0], &size) == nil &&
			strings.EqualFold(windows.UTF16ToString(buf[:size]), bin) {
			if windows.TerminateProcess(h, 1) == nil {
				n++
			}
		}
		windows.CloseHandle(h)
	}
	return n
}

// winDrives lists the fixed NTFS drives: where a VM's disk can live.
// Removable, optical and mapped network drives are left out; a disk that
// only looks fixed — an iSCSI volume, a USB disk — is listed and marked.
func winDrives() []drive {
	home := strings.ToUpper(filepath.VolumeName(os.Getenv("USERPROFILE")))
	mask, err := windows.GetLogicalDrives()
	if err != nil {
		return nil
	}
	var out []drive
	for i := 0; i < 26; i++ {
		if mask&(1<<uint(i)) == 0 {
			continue
		}
		letter := string(rune('A'+i)) + ":"
		root, _ := windows.UTF16PtrFromString(letter + `\`)
		if windows.GetDriveType(root) != windows.DRIVE_FIXED {
			continue
		}
		var fs [windows.MAX_PATH + 1]uint16
		if windows.GetVolumeInformation(root, nil, 0, nil, nil, nil, &fs[0], uint32(len(fs))) != nil ||
			windows.UTF16ToString(fs[:]) != "NTFS" {
			continue
		}
		var free, total uint64
		if windows.GetDiskFreeSpaceEx(root, &free, &total, nil) != nil {
			continue
		}
		d := drive{Letter: letter, Free: int64(free), Size: int64(total), Home: letter == home}
		d.Kind, d.Network = driveKind(letter)
		out = append(out, d)
	}
	return out
}

// driveBusSeen, when a test sets it, is told the bus type Windows reports.
var driveBusSeen func(letter string, bus uint32)

// driveKind asks the disk under a drive letter what it is: how it is
// attached, and whether it has to seek.
func driveKind(letter string) (kind string, network bool) {
	name, _ := windows.UTF16PtrFromString(`\\.\` + letter)
	h, err := windows.CreateFile(name, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return "", false
	}
	defer windows.CloseHandle(h)
	const ioctlStorageQueryProperty = 0x2D1400
	query := func(property uint32, out []byte) bool {
		in := struct {
			PropertyID, QueryType uint32
			Extra                 [4]byte
		}{PropertyID: property}
		var n uint32
		return windows.DeviceIoControl(h, ioctlStorageQueryProperty, (*byte)(unsafe.Pointer(&in)), uint32(unsafe.Sizeof(in)), &out[0], uint32(len(out)), &n, nil) == nil
	}
	// STORAGE_DEVICE_DESCRIPTOR: the bus type is the DWORD at offset 28
	desc := make([]byte, 1024)
	if query(0, desc) {
		bus := *(*uint32)(unsafe.Pointer(&desc[28]))
		if driveBusSeen != nil {
			driveBusSeen(letter, bus)
		}
		switch bus {
		case 16:
			// A Storage Space is Windows' own virtual disk; what matters
			// is what it is made of. The Storage module knows, and
			// PowerShell is the way to ask it without COM.
			return spaceKind(letter)
		case 9:
			return "network disk (iSCSI)", true
		case 6:
			return "network disk (Fibre Channel)", true
		case 7:
			return "USB disk", true
		case 12, 13:
			return "memory card", true
		}
	}
	// DEVICE_SEEK_PENALTY_DESCRIPTOR: the answer is the byte at offset 8
	seek := make([]byte, 16)
	if query(7, seek) {
		if seek[8] != 0 {
			return "hard disk", false
		}
		return "SSD", false
	}
	return "", false
}

// spaceKind says what a Storage Space is built on: a space whose disks
// are reached over a network, or a cable that can be pulled, is no better
// a home for a VM than those disks themselves.
func spaceKind(letter string) (kind string, network bool) {
	script := "(Get-PhysicalDisk -VirtualDisk (Get-VirtualDisk -Disk (Get-Partition -DriveLetter " + strings.TrimRight(letter, ":") +
		" | Get-Disk)) | ForEach-Object { [string]$_.BusType }) -join ','"
	out, err := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", script).Output()
	if err != nil {
		return "Storage Space", false
	}
	buses := strings.ToLower(strings.TrimSpace(string(out)))
	switch {
	case strings.Contains(buses, "iscsi"):
		return "Storage Space on a network disk (iSCSI)", true
	case strings.Contains(buses, "fibre"):
		return "Storage Space on a network disk (Fibre Channel)", true
	case strings.Contains(buses, "usb"):
		return "Storage Space on a USB disk", true
	}
	return "Storage Space", false
}
