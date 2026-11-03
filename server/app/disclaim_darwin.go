package main

// 蓝牙权限（TCC）算在「负责的进程」头上：被 gerdr 拉起时，负责的是 gerdr，要的是 gerdr 的蓝牙权限，
// gerdr 的 Info.plist 里没写蓝牙用途，系统会直接杀掉这个进程。
// 所以先用 responsibility_spawnattrs_setdisclaim 把自己重新起一遍：新进程自己负责自己，
// 用自己的标识（dev.bysir.miband-pet）和 Info.plist（App 包里的，或单独的二进制里嵌进去的）要蓝牙权限，
// 和 App 是同一份授权。终端、各种 IDE 起子进程也是这么做的。
//
// 原进程只是等新进程退出，转发信号，把退出码原样交出去；stdin / stdout / stderr 新进程直接继承。

/*
#include <spawn.h>
#include <stdlib.h>

int responsibility_spawnattrs_setdisclaim(posix_spawnattr_t *attrs, int disclaim);

static int spawn_disclaimed(const char *path, char *const argv[], char *const envp[], pid_t *pid) {
	posix_spawnattr_t attr;
	int rc = posix_spawnattr_init(&attr);
	if (rc != 0) return rc;
	rc = responsibility_spawnattrs_setdisclaim(&attr, 1);
	if (rc == 0) rc = posix_spawn(pid, path, NULL, &attr, argv, envp);
	posix_spawnattr_destroy(&attr);
	return rc;
}
*/
import "C"

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"unsafe"
)

const disclaimedEnv = "WRISTKIT_DISCLAIMED"

// disclaim 在需要时把自己重新起一遍，然后在这里退出；已经是重新起过的那个就直接返回。
func disclaim() {
	if os.Getenv(disclaimedEnv) == "1" {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		return // 起不了就照原样跑：从终端里跑的话终端有蓝牙权限也能用
	}
	cstrs := func(ss []string) (**C.char, func()) {
		arr := C.malloc(C.size_t(len(ss)+1) * C.size_t(unsafe.Sizeof(uintptr(0))))
		view := unsafe.Slice((**C.char)(arr), len(ss)+1)
		for i, s := range ss {
			view[i] = C.CString(s)
		}
		view[len(ss)] = nil
		return (**C.char)(arr), func() {
			for _, p := range view[:len(ss)] {
				C.free(unsafe.Pointer(p))
			}
			C.free(arr)
		}
	}
	argv, freeArgv := cstrs(os.Args)
	envp, freeEnvp := cstrs(append(os.Environ(), disclaimedEnv+"=1"))
	path := C.CString(exe)
	var pid C.pid_t
	rc := C.spawn_disclaimed(path, argv, envp, &pid)
	C.free(unsafe.Pointer(path))
	freeArgv()
	freeEnvp()
	if rc != 0 {
		fmt.Fprintln(os.Stderr, "wristkit: 重新启动自己失败，照原样运行:", syscall.Errno(rc))
		return
	}

	sig := make(chan os.Signal, 4)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	go func() {
		for s := range sig {
			syscall.Kill(int(pid), s.(syscall.Signal))
		}
	}()
	var ws syscall.WaitStatus
	for {
		_, err := syscall.Wait4(int(pid), &ws, 0, nil)
		if err != syscall.EINTR {
			break
		}
	}
	switch {
	case ws.Exited():
		os.Exit(ws.ExitStatus())
	case ws.Signaled():
		os.Exit(128 + int(ws.Signal()))
	}
	os.Exit(1)
}
