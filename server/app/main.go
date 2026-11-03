// wristkit 的 macOS App：Wails（系统 WKWebView）+ React。
// 打开 App 就在后台连手环、推送 agent 状态，界面显示状态和设置。构建见仓库根目录 Makefile。
package main

import (
	"embed"
	"log/slog"
	"os"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/mac"

	"miband/internal/bt"
)

//go:embed all:frontend/dist
var assets embed.FS

func main() {
	// wristkit serve / wristkit gerdr：不开窗口，后台运行（headless.go）
	if len(os.Args) > 1 && (os.Args[1] == "serve" || os.Args[1] == "gerdr") {
		mode := os.Args[1]
		disclaim() // 由 gerdr 等别的程序拉起时，蓝牙权限算在自己头上（disclaim_darwin.go）
		setupLog()
		bt.RunMain(func() int { return runHeadless(mode, os.Args[2:]) })
		return
	}
	setupLog()
	// IOBluetooth 的回调投递在主线程 run loop 上；Wails 的 App 本身就在主线程跑 NSApplication，不需要 bt.RunMain
	app := NewApp()
	err := wails.Run(&options.App{
		Title:            "wristkit",
		Width:            900,
		Height:           600,
		MinWidth:         760,
		MinHeight:        480,
		AssetServer:      &assetserver.Options{Assets: assets},
		BackgroundColour: &options.RGBA{R: 12, G: 14, B: 20, A: 255},
		OnStartup:        app.startup,
		OnShutdown:       app.shutdown,
		OnBeforeClose:    app.beforeClose,
		Bind:             []interface{}{app},
		Mac: &mac.Options{
			TitleBar: mac.TitleBarHiddenInset(),
			About: &mac.AboutInfo{
				Title:   "wristkit",
				Message: "在 Mac 上给小米手环写 app、装 app。\nhttps://github.com/zbysir/wristkit",
			},
		},
	})
	if err != nil {
		slog.Error("启动失败", "err", err)
	}
}
