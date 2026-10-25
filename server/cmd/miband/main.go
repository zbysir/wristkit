// miband：Mac 直连小米手环（经典蓝牙 SPP），把 app 装到手环上、给手环上的 app 推数据。
//
//	miband app                 常驻：跑 app 在 Mac 上的部分、给手环上开着的 app 推数据（Agent 猫会在 agent 跑完/卡住时提醒）
//	miband notify 标题 [正文]   发一条通知
//	miband battery             看手环电量
//	miband faces               列出手环上的表盘
//	miband face-install <文件>  安装表盘（.face / .bin）并设为当前
//	miband face-set <id>       切换到某个表盘
//	miband face-remove <id>    删除表盘
//	miband apps                列出手环上的第三方快应用
//	miband install <rpk>       安装快应用并打开
//	miband pack <目录> <rpk>     把 wristkit app 目录（manifest.json + index.ux）或快应用源码目录（manifest.json、app.ux、pages/…）
//	                           编译打包成安装包，不用 aiot-toolkit
//	miband launch <包名>        打开快应用
//	miband remove <包名>        卸载快应用
//	miband key                 扫码登录小米账号，拉手环的 auth key 和地址写进配置
//
// 配置 config.json：{"auth_key": "0x…", "address": "AA:BB:CC:DD:EE:FF", "notify": true, "notify_start": true, "alert": "auto"}
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"miband/internal/bandapp"
	"miband/internal/bt"
	"miband/internal/quickapp"
	"miband/internal/rpk"
	"miband/internal/service"
	"miband/internal/xiaomi"
)

func main() {
	bt.RunMain(run)
}

func run() int {
	configPath := flag.String("config", "", "配置文件路径（默认找 ./config.json，再找可执行文件上一级目录）")
	verbose := flag.Bool("v", false, "打印调试日志")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "用法: miband [-config path] [-v] <app|notify|battery|faces|face-install|face-set|face-remove|apps|install|remove|launch|pack|key> ...")
		flag.PrintDefaults()
	}
	flag.Parse()
	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	args := flag.Args()
	if len(args) == 0 {
		flag.Usage()
		return 2
	}
	if args[0] == "pack" {
		if len(args) < 3 {
			fatal(errors.New("用法: miband pack <源码目录> <rpk>"))
		}
		var data []byte
		var err error
		if _, statErr := os.Stat(filepath.Join(args[1], "index.ux")); statErr == nil {
			data, _, err = bandapp.BuildDir(os.DirFS(args[1]))
		} else {
			var s *rpk.Signer
			if s, err = quickapp.DebugSigner(); err == nil {
				data, _, err = quickapp.Build(os.DirFS(args[1]), s)
			}
		}
		if err == nil {
			err = os.WriteFile(args[2], data, 0o644)
		}
		if err != nil {
			fatal(err)
		}
		return 0
	}
	if args[0] == "key" {
		// 不需要已有配置：拉到 key 后写配置
		if err := runKey(*configPath); err != nil {
			fatal(err)
		}
		return 0
	}
	cfg, err := loadConfig(*configPath)
	if err != nil {
		fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch args[0] {
	case "app":
		var svc *service.Service
		if svc, err = service.New(*cfg); err == nil {
			err = svc.Run(ctx)
		}
	case "notify":
		if len(args) < 2 {
			fatal(errors.New("用法: miband notify 标题 [正文]"))
		}
		err = withDevice(ctx, cfg, func(dev *xiaomi.Device, _ *xiaomi.Apps) error {
			if err := dev.Notify(args[1], strings.Join(args[2:], " ")); err != nil {
				return err
			}
			time.Sleep(time.Second) // 给手环一点时间展示，再断开
			fmt.Println("已发送")
			return nil
		})
	case "battery":
		err = withDevice(ctx, cfg, func(dev *xiaomi.Device, _ *xiaomi.Apps) error {
			b, err := dev.Battery()
			if err != nil {
				return err
			}
			fmt.Printf("电量 %d%%，充电中: %v\n", b.Level, b.Charging)
			return nil
		})
	case "faces":
		err = withDevice(ctx, cfg, func(dev *xiaomi.Device, _ *xiaomi.Apps) error {
			list, err := dev.Watchfaces()
			for _, f := range list {
				mark := " "
				if f.Active {
					mark = "*"
				}
				fmt.Printf("%s %-12s 可删:%-5v %s\n", mark, f.ID, f.CanDelete, f.Name)
			}
			return err
		})
	case "face-install":
		if len(args) < 2 {
			fatal(errors.New("用法: miband face-install <文件>"))
		}
		data, rerr := os.ReadFile(args[1])
		if rerr != nil {
			fatal(rerr)
		}
		err = withDevice(ctx, cfg, func(dev *xiaomi.Device, _ *xiaomi.Apps) error {
			id, err := dev.InstallWatchface(data, func(i, n int) { fmt.Printf("\r传输 %d/%d", i, n) })
			fmt.Println()
			if err != nil {
				return err
			}
			fmt.Println("已安装并切换到表盘:", id)
			return nil
		})
	case "face-set", "face-remove":
		if len(args) < 2 {
			fatal(fmt.Errorf("用法: miband %s <id>", args[0]))
		}
		err = withDevice(ctx, cfg, func(dev *xiaomi.Device, _ *xiaomi.Apps) error {
			if args[0] == "face-set" {
				return dev.SetWatchface(args[1])
			}
			return dev.DeleteWatchface(args[1])
		})
	case "apps":
		err = withDevice(ctx, cfg, func(_ *xiaomi.Device, apps *xiaomi.Apps) error {
			list, err := apps.List()
			for _, a := range list {
				fmt.Printf("%-32s v%-4d %s\n", a.GetPackageName(), a.GetVersionCode(), a.GetAppName())
			}
			return err
		})
	case "launch":
		if len(args) < 2 {
			fatal(errors.New("用法: miband launch <包名>"))
		}
		pkg := args[1]
		err = withDevice(ctx, cfg, func(_ *xiaomi.Device, apps *xiaomi.Apps) error {
			if err := apps.Launch(pkg, ""); err != nil {
				return err
			}
			time.Sleep(time.Second)
			fmt.Println("已打开", pkg)
			return nil
		})
	case "remove":
		if len(args) < 2 {
			fatal(errors.New("用法: miband remove <包名>"))
		}
		err = withDevice(ctx, cfg, func(_ *xiaomi.Device, apps *xiaomi.Apps) error {
			if err := apps.Remove(args[1]); err != nil {
				return err
			}
			fmt.Println("已卸载", args[1])
			return nil
		})
	case "install":
		if len(args) < 2 {
			fatal(errors.New("用法: miband install <rpk>"))
		}
		err = withDevice(ctx, cfg, func(_ *xiaomi.Device, apps *xiaomi.Apps) error {
			m, err := apps.Install(args[1], func(i, n int) { fmt.Printf("\r传输 %d/%d", i, n) })
			fmt.Println()
			if err != nil {
				return err
			}
			fmt.Println("已安装:", m.Package)
			return apps.Launch(m.Package, "")
		})
	default:
		flag.Usage()
		return 2
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		fatal(err)
	}
	return 0
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "错误:", err)
	os.Exit(1)
}

func loadConfig(path string) (*service.Config, error) {
	candidates := []string{path}
	if path == "" {
		candidates = []string{"config.json"}
		if exe, err := os.Executable(); err == nil {
			candidates = append(candidates, filepath.Join(filepath.Dir(exe), "..", "config.json"))
		}
	}
	for _, p := range candidates {
		cfg, err := service.LoadConfig(p)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if err := cfg.Validate(); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		return cfg, nil
	}
	return nil, fmt.Errorf("找不到配置文件（试过 %s）", strings.Join(candidates, ", "))
}

// withDevice 连上手环执行 fn，结束（含 Ctrl-C）时一定关掉 RFCOMM 通道：
// 通道没关就退出，手环那边的会话会卡在半开状态，之后谁都连不上，只能重启手环。
func withDevice(ctx context.Context, cfg *service.Config, fn func(*xiaomi.Device, *xiaomi.Apps) error) error {
	dev, apps, err := service.NewDevice(*cfg)
	if err != nil {
		return err
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			dev.Close()
			os.Exit(130)
		case <-done:
		}
	}()
	// 刚断开上一个连接时手环偶尔不响应，重试一次
	if err := dev.Connect(); err != nil {
		slog.Warn("连接失败，3 秒后重试", "err", err)
		time.Sleep(3 * time.Second)
		if err := dev.Connect(); err != nil {
			return err
		}
	}
	defer dev.Close()
	return fn(dev, apps)
}
