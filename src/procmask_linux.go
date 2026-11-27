//go:build linux

package main

import (
	"os"
	"strings"
	"syscall"
	"unsafe"
)

// 进程伪装：prctl 改 comm（ps/top 的 COMMAND 列），
// argv 原地覆写（ps aux 的 CMD 列）。只挡随手一看，挡不住 /proc/<pid>/exe。

const prSetName = 15

// maybeReexec 用同名二进制重新 execve 自己，把 argv 整个换成想要的样子。
// 为什么不在原地改：argv[0] 的缓冲区只有原文件名的长度，写不下 "node server.js"，
// 跨过结束符去写会破坏环境变量区。re-exec 是同一 PID、一次性完成，最干净。
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
	// 走到这里说明 exec 失败，退回原地覆盖
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
