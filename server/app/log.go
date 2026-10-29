package main

// App 的日志写到 ~/Library/Logs/wristkit/wristkit.log（也照样打到 stderr）：手环断开、重连、手环上的 app 报错都在里面，
// 手环出问题（比如自己重启）以后能对着时间线查。满 5MB 换一个文件，只留上一个（wristkit.log.1）。

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

const logMax = 5 << 20

func setupLog() {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	dir := filepath.Join(home, "Library", "Logs", "wristkit")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	w := &rotatingFile{path: filepath.Join(dir, "wristkit.log")}
	if err := w.open(); err != nil {
		return
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(io.MultiWriter(os.Stderr, w), &slog.HandlerOptions{Level: slog.LevelInfo})))
	slog.Info("App 启动")
}

// rotatingFile 写满 logMax 就把当前文件改名成 .1，重新开一个
type rotatingFile struct {
	mu   sync.Mutex
	path string
	f    *os.File
	size int64
}

func (r *rotatingFile) open() error {
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	r.f, r.size = f, st.Size()
	return nil
}

func (r *rotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.size+int64(len(p)) > logMax {
		r.f.Close()
		_ = os.Rename(r.path, r.path+".1")
		if err := r.open(); err != nil {
			return 0, err
		}
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}
