//go:build linux

package main

import (
	"os"
	"strings"
	"syscall"
	"unsafe"
)


const prSetName = 15

func maybeReexec(mask string) {
	if os.Getenv("_CM_MASKED") == "1" {
		return
	}
	fields := splitSpace(mask)
	if len(fields) == 0 {
		return
	}
	env := append(os.Environ(), "_CM_MASKED=1")
	_ = syscall.Exec("/proc/self/exe", fields, env)
}

func splitSpace(s string) []string {
	out := []string{}
	for _, f := range strings.Fields(s) {
		out = append(out, f)
	}
	return out
}

func applyProcMask(title string) {
	setComm(title)
	setArgv(title)
}

func setComm(name string) {
	p, err := syscall.BytePtrFromString(name)
	if err != nil {
		return
	}
	_, _, _ = syscall.Syscall(syscall.SYS_PRCTL, uintptr(prSetName), uintptr(unsafe.Pointer(p)), 0)
}

func setArgv(title string) {
	defer func() { _ = recover() }()
	want := []byte(title)
	if len(want) == 0 {
		return
	}
	wi := 0
	for i := 0; i < len(os.Args); i++ {
		s := os.Args[i]
		if len(s) == 0 {
			continue
		}
		data := unsafe.StringData(s)
		if data == nil {
			continue
		}
		p := unsafe.Pointer(data)
		for j := 0; j < len(s); j++ {
			if wi < len(want) {
				*(*byte)(unsafe.Add(p, j)) = want[wi]
				wi++
			} else {
				*(*byte)(unsafe.Add(p, j)) = ' '
			}
		}
	}
}
