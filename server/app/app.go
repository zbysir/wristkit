package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"miband/apps"
	"miband/internal/micloud"
	"miband/internal/service"
	"miband/internal/tray"
	"miband/internal/xiaomi"
)

// App 是绑定给前端的对象：前端通过 window.go.main.App.* 调用这些导出方法。
type App struct {
	ctx     context.Context
	cfgPath string

	mu         sync.Mutex
	cfg        *service.Config
	svc        *service.Service
	stop       context.CancelFunc
	done       chan struct{}
	setupErr   string
	installing string
	progress   float64

	emitMu    sync.Mutex
	emitDue   bool
	storesDue bool

	quitting atomic.Bool // 从菜单栏“退出”时为 true；否则关窗口只是隐藏

	loginCancel context.CancelFunc // 进行中的扫码登录

	anim *trayAnim // 菜单栏的猫
}

func NewApp() *App {
	return &App{cfgPath: service.AppConfigPath(), anim: newTrayAnim()}
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	// 被 kill / launchd 停止时也走正常退出流程（OnShutdown 里关蓝牙通道）
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sig
		a.quit()
	}()
	tray.Init(a.showWindow, a.quit)
	go a.anim.run(ctx)
	a.bootstrapConfig()
	a.restartService()
}

// beforeClose：关窗口时只隐藏（连 Dock 图标一起），App 留在菜单栏继续推送；从菜单栏退出才真的退出。
func (a *App) beforeClose(ctx context.Context) bool {
	if a.quitting.Load() {
		return false
	}
	runtime.WindowHide(ctx)
	tray.SetDockVisible(false)
	return true
}

func (a *App) showWindow() {
	tray.SetDockVisible(true)
	runtime.WindowShow(a.ctx)
}

func (a *App) quit() {
	a.quitting.Store(true)
	runtime.Quit(a.ctx)
}

func (a *App) shutdown(context.Context) {
	a.stopService() // 一定要关掉蓝牙通道，否则手环那边会卡住
}

// bootstrapConfig：App 自己的配置不存在时，试着从 MIBAND_CONFIG 或仓库根目录的 config.json 复制一份。
func (a *App) bootstrapConfig() {
	if _, err := os.Stat(a.cfgPath); err == nil {
		return
	}
	candidates := []string{os.Getenv("MIBAND_CONFIG")}
	if wd, err := os.Getwd(); err == nil {
		candidates = append(candidates, filepath.Join(wd, "config.json"), filepath.Join(wd, "..", "..", "config.json"))
	}
	for _, p := range candidates {
		if p == "" {
			continue
		}
		cfg, err := service.LoadConfig(p)
		if err != nil || cfg.Validate() != nil {
			continue
		}
		if err := cfg.Save(a.cfgPath); err == nil {
			slog.Info("已导入配置", "from", p, "to", a.cfgPath)
			return
		}
	}
}

// stopService 停掉服务并关蓝牙通道。可能在主线程上调用（OnShutdown），
// 所以先直接关连接，再最多等 2 秒，不能无限等：服务里的蓝牙调用要派发到主线程，会互相卡死。
func (a *App) stopService() {
	a.mu.Lock()
	svc, stop, done := a.svc, a.stop, a.done
	a.svc, a.stop, a.done = nil, nil, nil
	a.mu.Unlock()
	if stop == nil {
		return
	}
	stop()
	svc.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		slog.Warn("等服务退出超时")
	}
}

func (a *App) restartService() {
	a.stopService()
	cfg, err := service.LoadConfig(a.cfgPath)
	if err == nil && cfg.MigrateLegacy() {
		// 旧版的提醒设置搬进了 Agent 猫的配置
		if err := cfg.Save(a.cfgPath); err != nil {
			slog.Warn("保存配置失败", "err", err)
		}
	}
	if err == nil {
		err = cfg.Validate()
	}
	var svc *service.Service
	if err == nil {
		svc, err = service.New(*cfg)
	}
	a.mu.Lock()
	a.cfg = cfg
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			a.setupErr = ""
		} else {
			a.setupErr = err.Error()
		}
		a.mu.Unlock()
		a.emitState()
		return
	}
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	svc.OnChange = a.emitState
	svc.OnStore = a.emitStores
	a.svc, a.stop, a.done, a.setupErr = svc, stop, done, ""
	a.mu.Unlock()
	go func() {
		defer close(done)
		svc.Run(ctx)
	}()
	a.emitState()
}

// emitStores 通知前端 app 数据变了（预览重新拉）；合并短时间内的多次变化。
func (a *App) emitStores() {
	a.emitMu.Lock()
	if a.storesDue {
		a.emitMu.Unlock()
		return
	}
	a.storesDue = true
	a.emitMu.Unlock()
	time.AfterFunc(100*time.Millisecond, func() {
		a.emitMu.Lock()
		a.storesDue = false
		a.emitMu.Unlock()
		if a.ctx != nil {
			runtime.EventsEmit(a.ctx, "stores")
		}
	})
}

// emitState 通知前端重新拉状态；合并短时间内的多次变化。
func (a *App) emitState() {
	a.emitMu.Lock()
	if a.emitDue {
		a.emitMu.Unlock()
		return
	}
	a.emitDue = true
	a.emitMu.Unlock()
	time.AfterFunc(150*time.Millisecond, func() {
		a.emitMu.Lock()
		a.emitDue = false
		a.emitMu.Unlock()
		if a.ctx != nil {
			runtime.EventsEmit(a.ctx, "state")
		}
		st := a.state()
		tray.SetInfo(trayText(st))
		a.anim.update(st)
	})
}

func (a *App) service() (*service.Service, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.svc == nil {
		return nil, errors.New("还没配置好手环")
	}
	return a.svc, nil
}

// trayText 是菜单栏菜单第一行的状态文字。
func trayText(st AppState) string {
	switch {
	case !st.Configured:
		return "还没配置手环"
	case !st.Connected && st.LastError != "":
		return "连接手环失败，重试中"
	case !st.Connected:
		return "正在连接手环…"
	}
	return "手环已连接"
}

// ---------- 小米账号：扫码登录拿 auth key ----------

// 登录态缓存：下次拉 key 不用再扫码（手环重新绑定后 key 会变）
func (a *App) sessionPath() string {
	return filepath.Join(filepath.Dir(a.cfgPath), "session.json")
}

// CachedDevices 用缓存的登录态拉账号下的设备；没有缓存或已过期时返回错误，前端改走扫码。
func (a *App) CachedDevices() ([]micloud.Device, error) {
	raw, err := os.ReadFile(a.sessionPath())
	if err != nil {
		return nil, errors.New("没有登录过")
	}
	var s micloud.Session
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return s.Devices(ctx)
}

type LoginResult struct {
	Devices []micloud.Device `json:"devices"`
	Error   string           `json:"error"`
}

// StartLogin 拿登录二维码（data URL 返回给前端显示），后台等扫码；结果通过 "login" 事件发给前端。
func (a *App) StartLogin() (string, error) {
	a.mu.Lock()
	if a.loginCancel != nil {
		a.loginCancel()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	a.loginCancel = cancel
	a.mu.Unlock()

	login, err := micloud.NewQRLogin(ctx)
	if err != nil {
		cancel()
		return "", err
	}
	go func() {
		defer cancel()
		res := LoginResult{}
		session, err := login.Wait(ctx)
		if err == nil {
			raw, _ := json.Marshal(session)
			os.MkdirAll(filepath.Dir(a.sessionPath()), 0o700)
			os.WriteFile(a.sessionPath(), raw, 0o600)
			res.Devices, err = session.Devices(context.Background())
		}
		if err != nil {
			if ctx.Err() == context.Canceled {
				return // 被新的登录取代了
			}
			res.Error = err.Error()
		}
		runtime.EventsEmit(a.ctx, "login", res)
	}()
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(login.QRImage), nil
}

func (a *App) CancelLogin() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.loginCancel != nil {
		a.loginCancel()
		a.loginCancel = nil
	}
}

// ---------- 绑定给前端的方法 ----------

type AppState struct {
	service.State
	Configured      bool    `json:"configured"`
	ConfigPath      string  `json:"configPath"`
	SetupError      string  `json:"setupError"`
	Installing      string  `json:"installing"`
	InstallProgress float64 `json:"installProgress"`
}

// GetState 返回 AppState 的 JSON 对象（前端的类型定义在 frontend/src/types.ts，
// 不让 Wails 生成 Go 内部包的类型）。
func (a *App) GetState() map[string]any {
	raw, _ := json.Marshal(a.state())
	var m map[string]any
	json.Unmarshal(raw, &m)
	return m
}

func (a *App) state() AppState {
	a.mu.Lock()
	svc := a.svc
	st := AppState{ConfigPath: a.cfgPath, SetupError: a.setupErr, Installing: a.installing, InstallProgress: a.progress}
	if a.cfg != nil {
		st.Configured = a.cfg.Validate() == nil
	}
	a.mu.Unlock()
	if svc != nil {
		st.State = svc.State()
	}
	return st
}

// SaveSetup 保存 auth key 和手环地址，然后重连。
func (a *App) SaveSetup(authKey, address string) error {
	authKey, address = strings.TrimSpace(authKey), strings.ToUpper(strings.TrimSpace(address))
	if !strings.HasPrefix(authKey, "0x") {
		authKey = "0x" + authKey
	}
	cfg := service.Config{}
	if old, err := service.LoadConfig(a.cfgPath); err == nil {
		cfg = *old
	}
	cfg.AuthKey, cfg.Address = authKey, address
	if err := cfg.Validate(); err != nil {
		return err
	}
	if _, err := xiaomi.NewDevice(cfg.AuthKey, cfg.Address, nil); err != nil {
		return err
	}
	if err := cfg.Save(a.cfgPath); err != nil {
		return err
	}
	a.restartService()
	return nil
}

// SetPet 换菜单栏的猫（手环上的猫在 Agent 猫的设置里）。
func (a *App) SetPet(skin string) error {
	svc, err := a.service()
	if err != nil {
		return err
	}
	cfg := svc.SetPet(skin)
	a.mu.Lock()
	a.cfg = &cfg
	a.mu.Unlock()
	return cfg.Save(a.cfgPath)
}

func (a *App) TestNotify() error {
	svc, err := a.service()
	if err != nil {
		return err
	}
	return svc.TestNotify()
}

func (a *App) Launch(pkg string) error {
	svc, err := a.service()
	if err != nil {
		return err
	}
	return svc.Launch(pkg)
}

// InstallApp 把 app 打包装（或更新）到手环上并打开。
func (a *App) InstallApp(name string) error {
	svc, err := a.service()
	if err != nil {
		return err
	}
	a.mu.Lock()
	if a.installing != "" {
		a.mu.Unlock()
		return errors.New("正在安装另一个 app")
	}
	a.installing, a.progress = name, 0
	a.mu.Unlock()
	a.emitState()
	defer func() {
		a.mu.Lock()
		a.installing, a.progress = "", 0
		a.mu.Unlock()
		a.emitState()
	}()
	return svc.InstallApp(name, func(i, n int) {
		a.mu.Lock()
		a.progress = float64(i) / float64(n)
		a.mu.Unlock()
		a.emitState()
	})
}

// OpenApp 在手环上打开这个 app。
func (a *App) OpenApp(name string) error {
	svc, err := a.service()
	if err != nil {
		return err
	}
	return svc.OpenApp(name)
}

// Uninstall 卸载手环上的快应用（app，或以前版本留下的旧应用）。
func (a *App) Uninstall(pkg string) error {
	svc, err := a.service()
	if err != nil {
		return err
	}
	return svc.Remove(pkg)
}

// OpenAppDir 在访达里打开用户 app 目录（没有就先建，放一份说明）。
func (a *App) OpenAppDir() error {
	dir := service.AppDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	readme := filepath.Join(dir, "README.md")
	if _, err := os.Stat(readme); os.IsNotExist(err) {
		_ = os.WriteFile(readme, []byte(appReadme), 0o644)
	}
	return exec.Command("open", dir).Run()
}

const appReadme = `# wristkit app 目录

每个子目录是一个 app（manifest.json + index.ux），保存后 App 里的预览马上更新；点「装到手环」装到手环上。
内置 app（Agent 猫）打包在 App 里，不在这个目录；想改的话，在 App 里点「复制出来改」，
会把源码复制到这里，同名 app 会替换内置的那个。

最简单的 app：

    hello/manifest.json   {"name": "hello", "title": "你好"}
    hello/index.ux

        <template>
          <div class="page" @click="add">
            <text class="n">点了 {{n}} 下</text>
          </div>
        </template>

        <script>
        export default {
          private: { n: 0 },
          add() { this.n++ }
        }
        </script>

        <style>
        .page { width: 212px; flex-direction: column; justify-content: center; align-items: center; }
        .n { font-size: 18px; color: #ffffff; }
        </style>

最省事的写法：让 AI 先读 https://github.com/zbysir/wristkit/blob/main/skills/wristkit-app/SKILL.md ，再告诉它你想在手环上看到什么。
完整说明：https://github.com/zbysir/wristkit/blob/main/docs/apps.md
`

// CopyBuiltin 把内置 app 的源码复制到用户 app 目录，方便改；同名的用户 app 会替换内置 app。
func (a *App) CopyBuiltin(name string) error {
	sub, err := fs.Sub(apps.FS, name)
	if err != nil {
		return err
	}
	if _, err := fs.Stat(sub, "manifest.json"); err != nil {
		return fmt.Errorf("没有内置 app %s", name)
	}
	dst := filepath.Join(service.AppDir(), name)
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("app 目录里已经有 %s 了", name)
	}
	err = fs.WalkDir(sub, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		target := filepath.Join(dst, filepath.FromSlash(p))
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := fs.ReadFile(sub, p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		return err
	}
	return exec.Command("open", dst).Run()
}

// ---------- app ----------

type AppPreview struct {
	App    string            `json:"app"`    // 编好的 app.js
	Page   string            `json:"page"`   // 编好的页面
	Assets map[string]string `json:"assets"` // 图片：路径（如 /assets/a.png）→ data URL
}

// AppPreview 给预览用：app 编好的代码（和装到手环上的是同一份）和图片。
func (a *App) AppPreview(name string) (*AppPreview, error) {
	svc, err := a.service()
	if err != nil {
		return nil, err
	}
	files, err := svc.AppFiles(name)
	if err != nil {
		return nil, err
	}
	p := &AppPreview{Assets: map[string]string{}}
	for _, f := range files {
		switch {
		case f.Name == "app.js":
			p.App = string(f.Body)
		case f.Name == "pages/index/index.js":
			p.Page = string(f.Body)
		case strings.HasSuffix(f.Name, ".png"):
			p.Assets["/"+f.Name] = "data:image/png;base64," + base64.StdEncoding.EncodeToString(f.Body)
		}
	}
	return p, nil
}

// AppStores 给预览用：所有 app 数据（JSON 字符串，前端自己解析，不让 Wails 转类型）。
func (a *App) AppStores() (string, error) {
	svc, err := a.service()
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(svc.AppStores())
	return string(raw), err
}

// AppCall 预览里 app 的 call(name, ...args)：和手环上发来的一样，交给 server.js 或 App 的功能。
func (a *App) AppCall(app, name string, argsJSON string) error {
	svc, err := a.service()
	if err != nil {
		return err
	}
	var args []any
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return err
	}
	return svc.AppCall(app, name, args)
}

// SetAppConfig 改 app 的一个配置项；value 是 JSON（前端传字符串，不让 Wails 转类型）。
func (a *App) SetAppConfig(app, key, valueJSON string) error {
	svc, err := a.service()
	if err != nil {
		return err
	}
	var v any
	if err := json.Unmarshal([]byte(valueJSON), &v); err != nil {
		return err
	}
	cfg, err := svc.SetAppConfig(app, key, v)
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.cfg = &cfg
	a.mu.Unlock()
	return cfg.Save(a.cfgPath)
}

// AllowApp 允许 app 的 server.js 用它要的权限；key 是界面上显示的那份权限（app 期间改了就不生效）。
func (a *App) AllowApp(name, key string) error {
	svc, err := a.service()
	if err != nil {
		return err
	}
	cfg := svc.AllowApp(name, key)
	a.mu.Lock()
	a.cfg = &cfg
	a.mu.Unlock()
	return cfg.Save(a.cfgPath)
}

// RevokeApp 收回 app 的权限。
func (a *App) RevokeApp(name string) error {
	svc, err := a.service()
	if err != nil {
		return err
	}
	cfg := svc.RevokeApp(name)
	a.mu.Lock()
	a.cfg = &cfg
	a.mu.Unlock()
	return cfg.Save(a.cfgPath)
}

// DownloadApp 从 GitHub 下载 app（地址是仓库里 app 目录的链接，或 owner/仓库/目录），返回 app 名字。
func (a *App) DownloadApp(input string) (string, error) {
	svc, err := a.service()
	if err != nil {
		return "", err
	}
	name, cfg, err := svc.DownloadApp(a.ctx, input)
	if err != nil {
		return "", err
	}
	return name, a.saveCfg(cfg)
}

// CheckAppUpdate：从 GitHub 下的 app 有没有新版本。
func (a *App) CheckAppUpdate(name string) (bool, error) {
	svc, err := a.service()
	if err != nil {
		return false, err
	}
	return svc.CheckAppUpdate(a.ctx, name)
}

// UpdateApp 从 GitHub 重新下载这个 app。
func (a *App) UpdateApp(name string) error {
	svc, err := a.service()
	if err != nil {
		return err
	}
	cfg, err := svc.UpdateApp(a.ctx, name)
	if err != nil {
		return err
	}
	return a.saveCfg(cfg)
}

// DeleteApp 把用户 app 移到废纸篓。
func (a *App) DeleteApp(name string) error {
	svc, err := a.service()
	if err != nil {
		return err
	}
	cfg, err := svc.DeleteApp(name)
	if err != nil {
		return err
	}
	return a.saveCfg(cfg)
}

func (a *App) saveCfg(cfg service.Config) error {
	a.mu.Lock()
	a.cfg = &cfg
	a.mu.Unlock()
	return cfg.Save(a.cfgPath)
}

// SetClaudeUsage 打开或关掉「显示 Claude Code 额度」：接管 Claude Code 的状态栏命令拿额度，关掉时原样还原。
func (a *App) SetClaudeUsage(on bool) error {
	svc, err := a.service()
	if err != nil {
		return err
	}
	return svc.SetClaudeUsage(on)
}
