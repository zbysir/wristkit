package bandapp

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"miband/internal/quickapp"
	"miband/internal/rpk"
)

// BaseStores 是基座给 server.js 的数据，所有 app 的 server.js 都能读；其它数据只有推它的 app 自己能读（app 之间不共用数据）。
//   - band：手环的状态 {connected, foreground}（foreground 是手环上这个 app 是否开在前台，见 Host.Foreground）
var BaseStores = []string{"band"}

// 单条消息不能太大：64KB 会让手环不再确认数据帧，连接卡死（docs/render-spike.md）。
// 大的数据分片发，每片不超过这么多字符，手环上的运行时拼回去。
const chunkChars = 6000

// Host 管理所有 app：编译打包，把数据推给手环上开着的 app，处理 app 发来的消息。
type Host struct {
	// Send 发一条消息给手环上的某个 app（按包名）。由使用方设置，会阻塞到发完。
	Send func(pkg string, v any) error
	// OnChange 在 app 列表、出错状态、前台状态变化时调用（给界面刷新用），不要阻塞。
	OnChange func()
	// Calls 是手环上的 app 能让 Mac 做的事（app 里 call(name, ...args)），按名字注册。
	// app 自己的 server.js 用 on(name) 注册的优先。
	Calls map[string]func(args []any) (any, error)
	// DataDir 存 server.js 的 storage 数据；为空则不持久化。
	DataDir string
	// NotifyBand 给手环发通知（server.js 的 notifyBand）。
	NotifyBand func(title, body string) error
	// Wake 把手环点亮并打开这个 app（server.js 的 wakeBand）。
	Wake func(app string) error
	// OnStore 在任何数据变化时调用（App 的预览用），不要阻塞。
	OnStore func(name string)
	// OnForeground 在手环上某个 app 打开（hello）或离开（bye）时调用。
	OnForeground func(app string, on bool)

	mu         sync.Mutex
	builtin    fs.FS
	userDir    string
	userSig    string
	apps       []*App
	stores     map[string]json.RawMessage
	runErr     map[string]string // 手环上报的运行错误：app 名 → 错误
	serverErr  map[string]string // server.js 的错误
	servers    map[string]*serverRuntime
	grants     map[string]string         // 用户允许过的权限：app 名 → Permissions.Key()（内置 app 不用问）
	configs    map[string]map[string]any // 用户改过的配置：app 名 → 配置项 → 值
	foreground map[string]bool           // 手环上开着的 app（收到 hello，还没 bye）
	lastPush   time.Time

	out  map[string]map[string]bool // 待发的数据：app 名 → 数据名（不带 app 前缀）
	kick chan struct{}
}

func NewHost() *Host {
	return &Host{
		Calls:      map[string]func([]any) (any, error){},
		stores:     map[string]json.RawMessage{},
		runErr:     map[string]string{},
		serverErr:  map[string]string{},
		servers:    map[string]*serverRuntime{},
		foreground: map[string]bool{},
		out:        map[string]map[string]bool{},
		kick:       make(chan struct{}, 1),
	}
}

// Load 加载内置 app（builtin 根目录下每个子目录一个 app）和用户 app 目录；同名的用户 app 替换内置 app。
func (h *Host) Load(builtin fs.FS, userDir string) {
	h.mu.Lock()
	h.builtin, h.userDir = builtin, userDir
	h.mu.Unlock()
	h.reload()
}

// Run 负责发送和用户 app 的热更新（改了重新编译；装到手环要用户点），一直跑到 ctx 结束。
func (h *Host) Run(ctx context.Context) {
	watch := time.NewTicker(2 * time.Second)
	defer watch.Stop()
	defer h.stopServers()
	for {
		select {
		case <-ctx.Done():
			return
		case <-h.kick:
			h.flush()
		case <-watch.C:
			if h.userChanged() {
				slog.Info("用户 app 有改动，重新编译")
				h.reload()
			}
		}
	}
}

func (h *Host) reload() {
	h.mu.Lock()
	builtin, userDir := h.builtin, h.userDir
	h.mu.Unlock()
	byName := map[string]*App{}
	add := func(fsys fs.FS, dir string, isBuiltin bool) {
		a, err := compile(fsys, dir, isBuiltin)
		if err != nil {
			slog.Warn("跳过 app", "dir", dir, "err", err)
			return
		}
		if a.Err != "" {
			slog.Warn("app 编译失败", "app", a.Name, "err", a.Err)
		}
		byName[a.Name] = a
	}
	if builtin != nil {
		entries, _ := fs.ReadDir(builtin, ".")
		for _, e := range entries {
			if sub, err := fs.Sub(builtin, e.Name()); err == nil && e.IsDir() {
				add(sub, e.Name(), true)
			}
		}
	}
	sig := ""
	if userDir != "" {
		entries, _ := os.ReadDir(userDir)
		for _, e := range entries {
			if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
				dir := filepath.Join(userDir, e.Name())
				add(os.DirFS(dir), dir, false)
			}
		}
		sig = dirSignature(userDir)
	}
	list := make([]*App, 0, len(byName))
	for _, a := range byName {
		list = append(list, a)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Order != list[j].Order {
			return list[i].Order < list[j].Order
		}
		return list[i].Name < list[j].Name
	})
	h.stopServers()
	h.mu.Lock()
	h.apps, h.userSig = list, sig
	h.runErr, h.serverErr = map[string]string{}, map[string]string{}
	h.mu.Unlock()
	h.publishConfigs()
	h.startServers()
	slog.Info("app 已加载", "count", len(list))
	h.changed()
}

// dirSignature：目录下所有文件的路径、大小、修改时间，用来发现用户 app 的改动
func dirSignature(dir string) string {
	var b strings.Builder
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			fmt.Fprintf(&b, "%s %d %d\n", p, info.Size(), info.ModTime().UnixNano())
		}
		return nil
	})
	return b.String()
}

func (h *Host) userChanged() bool {
	h.mu.Lock()
	dir, sig := h.userDir, h.userSig
	h.mu.Unlock()
	return dir != "" && dirSignature(dir) != sig
}

// startServers 跑各app 的 server.js：内置app 和没要权限的直接跑，要了权限的用户app 等用户允许。
func (h *Host) startServers() {
	h.mu.Lock()
	list := h.apps
	h.mu.Unlock()
	for _, p := range list {
		if p.ServerCode == "" || p.Err != "" || !h.allowed(p) {
			continue
		}
		m := startServer(h, p)
		h.mu.Lock()
		h.servers[p.Name] = m
		h.mu.Unlock()
	}
}

func (h *Host) stopServers() {
	h.mu.Lock()
	servers := h.servers
	h.servers = map[string]*serverRuntime{}
	h.mu.Unlock()
	for _, m := range servers {
		m.stop()
	}
}

func (h *Host) allowed(p *App) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.allowedLocked(p)
}

func (h *Host) allowedLocked(p *App) bool {
	return p.Builtin || p.Permissions.Empty() || h.grants[p.Name] == p.Permissions.Key()
}

// ---------- 配置 ----------

// SetConfigs 设置用户改过的配置（app 名 → 配置项 → 值），在 Load 之前调用。
func (h *Host) SetConfigs(configs map[string]map[string]any) {
	h.mu.Lock()
	h.configs = configs
	h.mu.Unlock()
	h.publishConfigs()
}

// SetConfig 改一个配置项（App 的设置表单），值不合法时返回错误。改完推给手环和 server.js。
func (h *Host) SetConfig(plugin, key string, value any) error {
	h.mu.Lock()
	var setting *Setting
	for _, p := range h.apps {
		if p.Name != plugin {
			continue
		}
		for i := range p.Settings {
			if p.Settings[i].Key == key {
				setting = &p.Settings[i]
			}
		}
	}
	if setting == nil {
		h.mu.Unlock()
		return fmt.Errorf("app  %s 没有配置项 %s", plugin, key)
	}
	v, err := setting.check(value)
	if err != nil {
		h.mu.Unlock()
		return err
	}
	if h.configs == nil {
		h.configs = map[string]map[string]any{}
	}
	if h.configs[plugin] == nil {
		h.configs[plugin] = map[string]any{}
	}
	h.configs[plugin][key] = v
	h.mu.Unlock()
	h.publishConfigs()
	h.changed()
	return nil
}

// configOf 是app 现在的配置：默认值 + 用户改过的（不合法的、manifest 里已经没有的项忽略）。调用方持有 mu。
func (h *Host) configOf(p *App) map[string]any {
	out := map[string]any{}
	for _, s := range p.Settings {
		out[s.Key] = s.Default
		if v, ok := h.configs[p.Name][s.Key]; ok {
			if v, err := s.check(v); err == nil {
				out[s.Key] = v
			}
		}
	}
	return out
}

// publishConfigs 把每个有配置项的app 的配置推成 "<app 名>:config"
func (h *Host) publishConfigs() {
	h.mu.Lock()
	cfgs := map[string]map[string]any{}
	for _, p := range h.apps {
		if len(p.Settings) > 0 {
			cfgs[p.Name] = h.configOf(p)
		}
	}
	h.mu.Unlock()
	for name, c := range cfgs {
		h.SetStore(name+":config", c)
	}
}

// CallAll 让所有在 server.js 里 on(name) 注册了的app 都执行一遍（比如 App 里开了 Claude 额度后让app 马上刷新）。
func (h *Host) CallAll(name string, args []any) {
	h.mu.Lock()
	servers := make([]*serverRuntime, 0, len(h.servers))
	for _, m := range h.servers {
		servers = append(servers, m)
	}
	h.mu.Unlock()
	for _, m := range servers {
		m.call(name, args)
	}
}

// SetGrants 设置用户允许过的权限（app 名 → Permissions.Key()），变了就重新跑 server.js。
func (h *Host) SetGrants(grants map[string]string) {
	h.mu.Lock()
	h.grants = grants
	running := h.servers != nil && len(h.apps) > 0
	h.mu.Unlock()
	if running {
		h.regrant()
	}
}

// regrant：权限变了，重新跑 server.js
func (h *Host) regrant() {
	h.stopServers()
	h.mu.Lock()
	h.serverErr = map[string]string{}
	h.mu.Unlock()
	h.startServers()
	h.changed()
}

func (h *Host) changed() {
	if h.OnChange != nil {
		h.OnChange()
	}
}

func (h *Host) markDirty() {
	select {
	case h.kick <- struct{}{}:
	default:
	}
}

// ---------- 数据和消息 ----------

// SetStore 设置一份数据。名字是「app 名:数据名」的（server.js 的 publish、配置）推给那个 app；
// 基座数据（BaseStores）只给 server.js 读。没变就不发。
func (h *Host) SetStore(name string, v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		slog.Warn("store 编码失败", "name", name, "err", err)
		return
	}
	h.mu.Lock()
	if string(h.stores[name]) == string(raw) {
		h.mu.Unlock()
		return
	}
	h.stores[name] = raw
	if app, key, ok := strings.Cut(name, ":"); ok {
		if h.out[app] == nil {
			h.out[app] = map[string]bool{}
		}
		h.out[app][key] = true
	}
	servers := make([]*serverRuntime, 0, len(h.servers))
	for _, m := range h.servers {
		servers = append(servers, m)
	}
	h.mu.Unlock()
	h.markDirty()
	for _, m := range servers {
		m.storeChanged(name, string(raw))
	}
	if h.OnStore != nil {
		h.OnStore(name)
	}
}

func (h *Host) storeJSON(name string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return string(h.stores[name])
}

// Stores 返回所有数据（App 的预览用）
func (h *Host) Stores() map[string]json.RawMessage {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make(map[string]json.RawMessage, len(h.stores))
	for k, v := range h.stores {
		out[k] = v
	}
	return out
}

// Call 处理 app 的 call(name, ...args)：先找 app 自己 server.js 里 on(name) 注册的，再找 Calls。
// 手环发来的和 App 预览里点的都走这里。
func (h *Host) Call(app, name string, args []any) error {
	h.mu.Lock()
	m := h.servers[app]
	fn := h.Calls[name]
	h.mu.Unlock()
	if m != nil && m.call(name, args) {
		return nil
	}
	if fn == nil {
		return fmt.Errorf("app %s 调用了没有的功能 %s", app, name)
	}
	_, err := fn(args)
	return err
}

// ByPackage 按手环上的包名找 app 的名字
func (h *Host) ByPackage(pkg string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, a := range h.apps {
		if a.Package == pkg {
			return a.Name
		}
	}
	return ""
}

// PackageOf 返回 app 在手环上的包名
func (h *Host) PackageOf(app string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if a := h.find(app); a != nil {
		return a.Package
	}
	return packageName(app)
}

func (h *Host) find(name string) *App {
	for _, a := range h.apps {
		if a.Name == name {
			return a
		}
	}
	return nil
}

// HandleMessage 处理手环上的 app（包名 pkg）发来的消息。
func (h *Host) HandleMessage(pkg string, m map[string]any) {
	app := h.ByPackage(pkg)
	if app == "" {
		return
	}
	switch m["t"] {
	case "hello":
		h.mu.Lock()
		h.foreground[app] = true
		if h.out[app] == nil {
			h.out[app] = map[string]bool{}
		}
		for name := range h.stores {
			if a, key, ok := strings.Cut(name, ":"); ok && a == app {
				h.out[app][key] = true
			}
		}
		h.mu.Unlock()
		h.markDirty()
		h.foregroundChanged(app, true)
	case "bye":
		h.mu.Lock()
		was := h.foreground[app]
		delete(h.foreground, app)
		h.mu.Unlock()
		if was {
			h.foregroundChanged(app, false)
		}
	case "call":
		name, _ := m["name"].(string)
		args, _ := m["args"].([]any)
		go func() {
			if err := h.Call(app, name, args); err != nil {
				slog.Info("app 调用失败", "app", app, "name", name, "err", err)
			}
		}()
	case "log":
		level, _ := m["level"].(string)
		msg, _ := m["msg"].(string)
		switch level {
		case "error":
			slog.Warn("app 出错", "app", app, "err", msg)
			h.mu.Lock()
			h.runErr[app] = msg
			h.mu.Unlock()
			h.changed()
		case "warn":
			slog.Warn("app", "app", app, "msg", msg)
		default:
			slog.Info("app", "app", app, "msg", msg)
		}
	}
}

func (h *Host) foregroundChanged(app string, on bool) {
	if h.OnForeground != nil {
		h.OnForeground(app, on)
	}
	h.changed()
}

// Foreground 返回手环上开着的 app；没有返回空串
func (h *Host) Foreground() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, a := range h.apps {
		if h.foreground[a.Name] {
			return a.Name
		}
	}
	return ""
}

// Disconnected：蓝牙断了，手环上的 app 肯定都不在前台了。
func (h *Host) Disconnected() {
	h.mu.Lock()
	var was []string
	for name := range h.foreground {
		was = append(was, name)
	}
	h.foreground = map[string]bool{}
	h.mu.Unlock()
	for _, name := range was {
		h.foregroundChanged(name, false)
	}
}

// ---------- 发送 ----------

// flush 把待发的数据发给手环上开着的 app。没开着的先攒着，打开时（hello）全部重发。
func (h *Host) flush() {
	type msg struct {
		pkg string
		v   any
	}
	h.mu.Lock()
	if h.Send == nil {
		h.mu.Unlock()
		return
	}
	var msgs []msg
	for _, a := range h.apps {
		names := h.out[a.Name]
		if !h.foreground[a.Name] || len(names) == 0 {
			continue
		}
		list := make([]string, 0, len(names))
		for name := range names {
			list = append(list, name)
		}
		sort.Strings(list)
		for _, name := range list {
			raw := h.stores[a.Name+":"+name]
			if raw == nil {
				continue
			}
			for _, v := range splitStore(name, raw) {
				msgs = append(msgs, msg{a.Package, v})
			}
		}
		delete(h.out, a.Name)
	}
	h.mu.Unlock()

	for i, m := range msgs {
		if err := h.Send(m.pkg, m.v); err != nil {
			slog.Warn("发给手环失败", "err", err, "left", len(msgs)-i)
			return // 没发出去的下次打开（hello）时会重发
		}
	}
	if len(msgs) > 0 {
		h.mu.Lock()
		h.lastPush = time.Now()
		h.mu.Unlock()
		slog.Debug("已发给手环", "messages", len(msgs))
	}
}

var partSeq int

// splitStore：一份数据一条消息；太大的拆成几片（{t: part}），手环上拼回去再当 store 处理
func splitStore(name string, raw json.RawMessage) []any {
	if len(raw) <= chunkChars {
		return []any{map[string]any{"t": "store", "name": name, "v": raw}}
	}
	partSeq++
	s := string(raw)
	n := (len(s) + chunkChars - 1) / chunkChars
	out := make([]any, n)
	for k := range n {
		out[k] = map[string]any{"t": "part", "id": partSeq, "name": name, "k": k, "n": n, "s": s[k*chunkChars : min((k+1)*chunkChars, len(s))]}
	}
	return out
}

// Files 返回 app 编好的安装包里的文件（还没签名，预览用）
func (h *Host) Files(name string) ([]rpk.File, error) {
	h.mu.Lock()
	a := h.find(name)
	h.mu.Unlock()
	if a == nil {
		return nil, fmt.Errorf("没有 app %s", name)
	}
	if a.Err != "" {
		return nil, fmt.Errorf("%s", a.Err)
	}
	return a.Files, nil
}

// Build 打出 app 签好名的安装包
func (h *Host) Build(name string) ([]byte, *App, error) {
	h.mu.Lock()
	a := h.find(name)
	h.mu.Unlock()
	if a == nil {
		return nil, nil, fmt.Errorf("没有 app %s", name)
	}
	if a.Err != "" {
		return nil, a, fmt.Errorf("%s", a.Err)
	}
	s, err := quickapp.DebugSigner()
	if err != nil {
		return nil, a, err
	}
	data, err := rpk.Build(a.Files, "", s)
	return data, a, err
}

// ---------- 给界面看的状态 ----------

type Info struct {
	Name        string         `json:"name"`
	Title       string         `json:"title"`
	Builtin     bool           `json:"builtin"`
	Dir         string         `json:"dir"`
	Source      string         `json:"source"` // 从 GitHub 下的：来源链接
	Error       string         `json:"error"`
	Package     string         `json:"package"`    // 手环上的包名
	Version     int            `json:"version"`    // 安装包的版本号，和手环上装的不一样就要更新
	Foreground  bool           `json:"foreground"` // 手环上正开着
	HasServer   bool           `json:"hasServer"`
	Permissions []string       `json:"permissions"` // server.js 要的权限（给人看的）
	NeedGrant   bool           `json:"needGrant"`   // 要了权限还没被允许，server.js 没跑
	PermKey     string         `json:"permKey"`
	Settings    []Setting      `json:"settings"` // 配置项
	Config      map[string]any `json:"config"`   // 现在的配置
}

func (h *Host) Apps() []Info {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]Info, len(h.apps))
	for i, a := range h.apps {
		err := a.Err
		if err == "" {
			err = h.runErr[a.Name]
		}
		if err == "" {
			err = h.serverErr[a.Name]
		}
		hasServer := a.ServerCode != ""
		out[i] = Info{
			Name: a.Name, Title: a.Title, Builtin: a.Builtin, Dir: a.Dir, Source: a.Source, Error: err, Package: a.Package, Version: a.Version,
			Foreground: h.foreground[a.Name], HasServer: hasServer, Permissions: a.Permissions.Describe(), PermKey: a.Permissions.Key(),
			NeedGrant: hasServer && !h.allowedLocked(a),
			Settings:  a.Settings, Config: h.configOf(a),
		}
	}
	return out
}

// LastPush 最近一次给手环发东西的时间。
func (h *Host) LastPush() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.lastPush
}
