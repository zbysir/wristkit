// Package service 是常驻服务：连着手环，管理 app（server/internal/bandapp）：编译、装到手环、给手环上开着的 app 推数据。
// 读 agent 状态、提醒这些都是 app 的事（见 server/apps/agent-cat）。命令行（miband app）和 macOS App 共用。
package service

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"miband/apps"
	"miband/internal/bandapp"
	"miband/internal/bt"
	"miband/internal/rpk"
	"miband/internal/usage"
	"miband/internal/xiaomi"
)

const (
	reconnectInterval    = 5 * time.Second // 断开后第一次重连的间隔，连续失败就翻倍
	maxReconnectInterval = time.Minute
)

// LegacyPackages 是以前版本装到手环上的快应用，已经不再用，界面上提示卸载：
// 最早的 Agents 列表、Agent 猫，和后来所有插件共用的通用渲染器（每个 app 单独装成快应用之后就不要了）。
var LegacyPackages = []string{"dev.bysir.herdrband", "dev.bysir.herdrpet", "dev.bysir.mibandpet"}

// AppDir 是用户 app 目录：~/Library/Application Support/miband-pet/apps/<名字>/
func AppDir() string {
	return filepath.Join(filepath.Dir(AppConfigPath()), "apps")
}

// Dial 连手环：先走经典蓝牙 RFCOMM（日常连接）；连不上就扫描 BLE 再连。
// 手环在「连接新手机」的二维码界面时只能通过 BLE 连上（首次接管就靠这个）。
func Dial(address string, onData func([]byte), onClosed func()) (xiaomi.Transport, error) {
	t, _, err := dial(address, "", onData, onClosed)
	return t, err
}

// dial 按 prefer（上次成功的方式）优先尝试：经典蓝牙连不上时要等到超时（十几秒），
// 首次接管走通 BLE 之后就别每次都先等经典蓝牙超时了。
func dial(address, prefer string, onData func([]byte), onClosed func()) (xiaomi.Transport, string, error) {
	classic := func() (xiaomi.Transport, error) {
		conn, _, err := bt.Open(address, onData, onClosed)
		if err != nil {
			return nil, err // 不能返回 nil 的 *bt.Conn 包成非 nil 的接口
		}
		return conn, nil
	}
	ble := func() (xiaomi.Transport, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		conn, err := bt.OpenBLE(ctx, address, onData, onClosed)
		if err != nil {
			return nil, err
		}
		return conn, nil
	}
	order := []string{"classic", "ble"}
	if prefer == "ble" {
		order = []string{"ble", "classic"}
	}
	var errs []string
	for _, kind := range order {
		open := classic
		if kind == "ble" {
			open = ble
		}
		t, err := open()
		if err == nil {
			return t, kind, nil
		}
		slog.Info("连接方式失败，换另一种", "transport", kind, "err", err)
		errs = append(errs, err.Error())
	}
	return nil, "", errors.New(strings.Join(errs, "；"))
}

// NewDevice 按配置创建手环会话（还没连接）。
func NewDevice(cfg Config) (*xiaomi.Device, *xiaomi.Apps, error) {
	if err := cfg.Validate(); err != nil {
		return nil, nil, err
	}
	dev, err := xiaomi.NewDevice(cfg.AuthKey, cfg.Address, Dial)
	if err != nil {
		return nil, nil, err
	}
	return dev, xiaomi.NewApps(dev), nil
}

// State 是给界面看的快照。
type State struct {
	Connected  bool            `json:"connected"`
	Transport  string          `json:"transport"` // classic / ble
	LastError  string          `json:"lastError"`
	Packages   []string        `json:"packages"`   // 手环上装了的 app（包名）
	Versions   map[string]int  `json:"versions"`   // 手环上装了的快应用 → 版本号（versionCode）
	Foreground string          `json:"foreground"` // 手环上正在前台的快应用
	Usage      []usage.Source  `json:"usage"`
	Apps       []bandapp.Info  `json:"apps"`
	AppDir     string          `json:"appDir"`
	Legacy     []string        `json:"legacy"`   // 手环上还装着的旧版快应用
	LastPush   int64           `json:"lastPush"` // 毫秒
	Battery    *xiaomi.Battery `json:"battery"`  // 手环电量；没读到是 null
	Settings   map[string]any  `json:"settings"`
}

type Service struct {
	dev        *xiaomi.Device
	apps       *xiaomi.Apps
	host       *bandapp.Host
	statusline usage.Statusline // 接管 Claude Code 的状态栏拿额度（App 里打开）
	// 状态变化时回调（界面用来刷新），不要在里面阻塞
	OnChange func()
	// app 数据（store）变化时回调（App 的预览用），不要在里面阻塞
	OnStore func()

	mu         sync.Mutex
	cfg        Config
	packages   []string        // 手环上装了的 app（包名）
	versions   map[string]int  // 手环上装了的快应用 → 版本号
	legacy     []string        // 手环上还装着的旧版快应用
	foreground map[string]bool // 快应用打开时发 hello、离开时发 bye
	lastErr    string
	transport  string
	battery    *xiaomi.Battery
	batteryAt  time.Time // 上次读电量的时间

	dirty chan struct{} // 连接断了，叫醒重连循环
}

func New(cfg Config) (*Service, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	cfg.MigrateLegacy()
	dataDir := filepath.Dir(AppConfigPath())
	s := &Service{
		cfg: cfg, foreground: map[string]bool{}, dirty: make(chan struct{}, 1),
		statusline: usage.DefaultStatusline(dataDir),
	}
	dev, err := xiaomi.NewDevice(cfg.AuthKey, cfg.Address, func(address string, onData func([]byte), onClosed func()) (xiaomi.Transport, error) {
		s.mu.Lock()
		prefer := s.transport
		s.mu.Unlock()
		t, kind, err := dial(address, prefer, onData, onClosed)
		if err == nil {
			s.mu.Lock()
			s.transport = kind
			s.mu.Unlock()
		}
		return t, err
	})
	if err != nil {
		return nil, err
	}
	apps := xiaomi.NewApps(dev)
	s.dev, s.apps = dev, apps
	s.host = bandapp.NewHost()
	s.host.Send = func(pkg string, v any) error {
		if !s.dev.Connected() {
			return errNotConnected
		}
		return s.apps.Send(pkg, v)
	}
	s.host.OnForeground = func(app string, on bool) { s.setForeground(s.host.PackageOf(app), on) }
	s.host.OnChange = s.changed
	s.host.OnStore = func(string) {
		if s.OnStore != nil {
			s.OnStore()
		}
	}
	s.host.DataDir = filepath.Join(dataDir, "plugin-data")
	s.host.NotifyBand = func(title, body string) error {
		if !s.dev.Connected() {
			return errNotConnected
		}
		return s.dev.Notify(title, body)
	}
	// 「显示 Claude Code 额度」开着的话，状态栏脚本换成这一版，清掉一周没动静的会话文件
	if err := s.statusline.Refresh(7 * 24 * time.Hour); err != nil {
		slog.Warn("更新状态栏脚本失败", "err", err)
	}
	s.host.Wake = s.wake
	s.host.SetGrants(copyGrants(cfg.PluginGrants))
	s.host.SetConfigs(copyConfigs(cfg.PluginConfig))
	s.publishBand()
	apps.OnMessage = func(pkg string, payload any) {
		m, _ := payload.(map[string]any)
		if !strings.HasPrefix(pkg, bandapp.PackagePrefix) || m == nil {
			return
		}
		s.host.HandleMessage(pkg, m)
	}
	dev.OnBattery = func(b xiaomi.Battery) { s.setBattery(&b) }
	dev.OnDisconnect = func() {
		s.setBattery(nil)
		s.host.Disconnected()
		s.publishBand()
		s.changed()
		s.markDirty() // 让 keepConnected 醒过来去重连
	}
	return s, nil
}

// Run 一直跑到 ctx 结束；结束时关掉蓝牙通道。
func (s *Service) Run(ctx context.Context) error {
	defer s.dev.Close()
	s.host.Load(apps.FS, AppDir())
	go s.host.Run(ctx)
	s.keepConnected(ctx)
	return ctx.Err()
}

// Close 立刻关掉蓝牙连接。退出时在主线程上调用：这时不能等 Run 结束，
// 因为 Run 里的蓝牙调用要派发到主线程，主线程在等就会死锁。
func (s *Service) Close() {
	s.dev.Close()
}

func (s *Service) changed() {
	if s.OnChange != nil {
		s.OnChange()
	}
}

func (s *Service) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := State{
		Connected: s.dev.Connected(),
		Transport: s.transport,
		Battery:   s.battery,
		LastError: s.lastErr,
		Packages:  append([]string{}, s.packages...),
		Versions:  maps.Clone(s.versions),
		Legacy:    append([]string{}, s.legacy...),
		Usage:     s.usageLocked(),
		Apps:      s.host.Apps(),
		AppDir:    AppDir(),
		Settings: map[string]any{
			"address": s.cfg.Address, "pet": s.cfg.PetSkin(), "claudeUsage": s.statusline.Enabled(),
		},
	}
	st.Foreground = s.foregroundLocked()
	if t := s.host.LastPush(); !t.IsZero() {
		st.LastPush = t.UnixMilli()
	}
	return st
}

// SetPet 换菜单栏的猫（手环上的猫是 Agent 猫 app 自己的配置，两个分开）。
func (s *Service) SetPet(skin string) Config {
	s.mu.Lock()
	s.cfg.Pet = skin
	s.cfg.Pet = s.cfg.PetSkin() // 不认识的值回落到橘猫
	cfg := s.cfg
	s.mu.Unlock()
	s.changed()
	return cfg
}

// SetClaudeUsage 打开或关掉「显示 Claude Code 额度」（接管 Claude Code 的状态栏命令，见 usage.Statusline）。
func (s *Service) SetClaudeUsage(on bool) error {
	var err error
	if on {
		err = s.statusline.Enable()
	} else {
		err = s.statusline.Disable()
	}
	// 让读额度的 app（server.js 里 on('refresh')）马上重新读一次
	s.host.CallAll("refresh", nil)
	s.changed()
	return err
}

// usageLocked 汇总各 app 的 server.js 推的额度（数据名是 usage 的，即 "<app 名>:usage"），给 App 界面看
func (s *Service) usageLocked() []usage.Source {
	var out []usage.Source
	stores := s.host.Stores()
	names := make([]string, 0, len(stores))
	for k := range stores {
		if strings.HasSuffix(k, ":usage") {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	for _, k := range names {
		var list []usage.Source
		if json.Unmarshal(stores[k], &list) == nil {
			out = append(out, list...)
		}
	}
	return out
}

// SetAppConfig 改 app 的一个配置项（App 的设置表单）。
func (s *Service) SetAppConfig(app, key string, value any) (Config, error) {
	if err := s.host.SetConfig(app, key, value); err != nil {
		s.mu.Lock()
		cfg := s.cfg
		s.mu.Unlock()
		return cfg, err
	}
	s.mu.Lock()
	if s.cfg.PluginConfig == nil {
		s.cfg.PluginConfig = map[string]map[string]any{}
	}
	if s.cfg.PluginConfig[app] == nil {
		s.cfg.PluginConfig[app] = map[string]any{}
	}
	s.cfg.PluginConfig[app][key] = value
	cfg := s.cfg
	s.mu.Unlock()
	s.changed()
	return cfg, nil
}

func copyConfigs(m map[string]map[string]any) map[string]map[string]any {
	out := make(map[string]map[string]any, len(m))
	for k, v := range m {
		c := make(map[string]any, len(v))
		for kk, vv := range v {
			c[kk] = vv
		}
		out[k] = c
	}
	return out
}

// ---------- app 预览（App 窗口里跑 app） ----------

// AppFiles 返回 app 编好的安装包里的文件（预览用）。
func (s *Service) AppFiles(name string) ([]rpk.File, error) {
	return s.host.Files(name)
}

// AppStores 返回所有 app 数据。
func (s *Service) AppStores() map[string]json.RawMessage {
	return s.host.Stores()
}

// AppCall 处理预览里 app 的 call(name, ...args)，和手环上发来的走同一条路。
func (s *Service) AppCall(app, name string, args []any) error {
	return s.host.Call(app, name, args)
}

// AllowApp 允许 app 的 server.js 用它在 manifest.json 里要的权限。key 是界面上显示时的权限，
// 和 app 现在要的不一样（这期间 app 改了）就不生效。
func (s *Service) AllowApp(name, key string) Config {
	s.mu.Lock()
	if s.cfg.PluginGrants == nil {
		s.cfg.PluginGrants = map[string]string{}
	}
	s.cfg.PluginGrants[name] = key
	cfg := s.cfg
	grants := copyGrants(s.cfg.PluginGrants)
	s.mu.Unlock()
	s.host.SetGrants(grants)
	s.changed()
	return cfg
}

// RevokeApp 收回 app 的权限，它的 server.js 停掉。
func (s *Service) RevokeApp(name string) Config {
	s.mu.Lock()
	delete(s.cfg.PluginGrants, name)
	cfg := s.cfg
	grants := copyGrants(s.cfg.PluginGrants)
	s.mu.Unlock()
	s.host.SetGrants(grants)
	s.changed()
	return cfg
}

func copyGrants(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func (s *Service) setForeground(pkg string, on bool) {
	s.mu.Lock()
	if on {
		s.foreground[pkg] = true
	} else {
		delete(s.foreground, pkg)
	}
	s.mu.Unlock()
	slog.Debug("快应用前台状态", "package", pkg, "foreground", on)
	s.publishBand()
	s.changed()
}

// publishBand 把手环的状态推给 server.js（基座数据 band，见 bandapp.BaseStores）：
// connected 手环连着没有，foreground 手环上有没有 app 开在前台
func (s *Service) publishBand() {
	s.mu.Lock()
	fg := s.foregroundLocked() != ""
	s.mu.Unlock()
	s.host.SetStore("band", map[string]bool{"connected": s.dev.Connected(), "foreground": fg})
}

// foregroundLocked 返回手环上正在前台的快应用；没有则返回空串。调用方持有 mu。
func (s *Service) foregroundLocked() string {
	for _, p := range s.packages {
		if s.foreground[p] {
			return p
		}
	}
	return ""
}

func (s *Service) markDirty() {
	select {
	case s.dirty <- struct{}{}:
	default:
	}
}

func (s *Service) setErr(err error) {
	s.mu.Lock()
	if err != nil {
		s.lastErr = err.Error()
	} else {
		s.lastErr = ""
	}
	s.mu.Unlock()
	s.changed()
}

// refreshApps 重新读手环上装了哪些快应用，并告诉它们“手机端”在线。
func (s *Service) refreshApps() error {
	items, err := s.apps.List()
	if err != nil {
		return err
	}
	installed := map[string]bool{}
	versions := map[string]int{}
	for _, it := range items {
		installed[it.GetPackageName()] = true
		versions[it.GetPackageName()] = int(it.GetVersionCode())
	}
	var list, legacy []string
	for p := range installed {
		if strings.HasPrefix(p, bandapp.PackagePrefix) {
			list = append(list, p)
		}
	}
	sort.Strings(list)
	for _, p := range LegacyPackages {
		if installed[p] {
			legacy = append(legacy, p)
		}
	}
	s.mu.Lock()
	s.packages, s.legacy, s.versions = list, legacy, versions
	s.foreground = map[string]bool{} // 由 app 重新发 hello
	s.mu.Unlock()
	s.publishBand()
	slog.Info("手环上的快应用", "packages", list, "versions", versions)
	for _, p := range list {
		if err := s.apps.SetStatus(p, true); err != nil {
			return err
		}
	}
	return nil
}

// waitAuthorized 等蓝牙授权。没授权时去碰 IOBluetooth 会阻塞到用户点完弹窗（还卡住主线程），所以先等。
func (s *Service) waitAuthorized(ctx context.Context) bool {
	for ctx.Err() == nil {
		switch bt.GetAuthorization() {
		case bt.AuthAllowed:
			return true
		case bt.AuthNotDetermined:
			bt.RequestAuthorization()
			s.setErr(errors.New("请在系统弹窗里允许 wristkit 使用蓝牙"))
		default:
			s.setErr(errors.New("没有蓝牙权限：到 系统设置 → 隐私与安全性 → 蓝牙 里允许 wristkit"))
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
	return false
}

func (s *Service) ensureConnected(ctx context.Context) {
	if !s.waitAuthorized(ctx) {
		return
	}
	// 手环长时间不在旁边（或连着手机）时别一直猛试：间隔从 5 秒翻倍到 1 分钟，连上后重置
	wait := reconnectInterval
	for !s.dev.Connected() && ctx.Err() == nil {
		err := s.dev.Connect()
		if err == nil {
			err = s.refreshApps()
		}
		s.setErr(err)
		if err == nil {
			return
		}
		slog.Warn("连接手环失败，稍后重试", "err", err, "retry", wait)
		s.dev.Close()
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(wait*2, maxReconnectInterval)
	}
}

// keepConnected 一直保持连着手环：断了就重连。数据由 app 宿主自己推。
func (s *Service) keepConnected(ctx context.Context) {
	for ctx.Err() == nil {
		s.ensureConnected(ctx)
		s.refreshBattery()
		select {
		case <-ctx.Done():
			return
		case <-s.dirty: // 断开了
		case <-time.After(time.Minute): // 定期回到 ensureConnected 检查连接
		}
	}
}

// 隔多久读一次手环电量
const batteryInterval = 10 * time.Minute

// refreshBattery 隔 batteryInterval 读一次电量。有的手环不回电量，就不显示。
func (s *Service) refreshBattery() {
	s.mu.Lock()
	due := time.Since(s.batteryAt) >= batteryInterval
	if due {
		s.batteryAt = time.Now()
	}
	s.mu.Unlock()
	if !due || !s.dev.Connected() {
		return
	}
	b, err := s.dev.Battery()
	if err != nil {
		slog.Debug("读手环电量失败", "err", err)
		return
	}
	s.setBattery(&b)
}

func (s *Service) setBattery(b *xiaomi.Battery) {
	s.mu.Lock()
	if b == nil {
		s.batteryAt = time.Time{} // 重连后马上再读
	}
	s.battery = b
	s.mu.Unlock()
	s.changed()
}

// ---------- 界面操作 ----------

var errNotConnected = errors.New("手环未连接")

func (s *Service) TestNotify() error {
	if !s.dev.Connected() {
		return errNotConnected
	}
	return s.dev.Notify("wristkit", "测试通知 "+time.Now().Format("15:04:05"))
}

// wake 把手环点亮并打开这个 app（server.js 的 wakeBand）：手环会亮屏、不震动。
func (s *Service) wake(name string) error {
	if !s.dev.Connected() {
		return errNotConnected
	}
	pkg := s.host.PackageOf(name)
	s.mu.Lock()
	installed := s.versions[pkg] > 0
	s.mu.Unlock()
	if !installed {
		return errors.New("手环上还没装这个 app")
	}
	return s.apps.Launch(pkg, "")
}

func (s *Service) Launch(pkg string) error {
	if !s.dev.Connected() {
		return errNotConnected
	}
	return s.apps.Launch(pkg, "")
}

// OpenApp 在手环上打开这个 app。
func (s *Service) OpenApp(name string) error {
	return s.Launch(s.host.PackageOf(name))
}

// InstallApp 把 app 打包装到手环上（装过就更新）并打开。
func (s *Service) InstallApp(name string, progress func(i, n int)) error {
	data, _, err := s.host.Build(name)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp("", "wristkit-*.rpk")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	f.Close()
	if err := s.Install(f.Name(), progress); err != nil {
		return err
	}
	s.host.ClearRunErr(name)
	return nil
}

// Remove 卸载手环上的快应用（用来清掉旧版）。
func (s *Service) Remove(pkg string) error {
	if !s.dev.Connected() {
		return errNotConnected
	}
	if err := s.apps.Remove(pkg); err != nil {
		return err
	}
	time.Sleep(500 * time.Millisecond) // 等手环删完再刷新列表
	if err := s.refreshApps(); err != nil {
		return err
	}
	s.changed()
	return nil
}

// Install 装（或更新）快应用并打开。
func (s *Service) Install(path string, progress func(i, n int)) error {
	if !s.dev.Connected() {
		return errNotConnected
	}
	m, err := s.apps.Install(path, progress)
	if err != nil {
		return err
	}
	if err := s.refreshApps(); err != nil {
		return err
	}
	s.changed()
	return s.apps.Launch(m.Package, "")
}
