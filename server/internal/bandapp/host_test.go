package bandapp

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/dop251/goja"
	"github.com/dop251/goja_nodejs/eventloop"

	"miband/apps"
)

var catPkg = packageName("agent-cat")

// ---------- 假的 Mac 发送端：记下 Host 发给手环的消息 ----------

type fakeBand struct {
	mu   sync.Mutex
	msgs []map[string]any
	// to 非空时把消息交给模拟的手环 app（同一个包名的）
	to map[string]*bandSim
}

func (f *fakeBand) send(pkg string, v any) error {
	raw, _ := json.Marshal(v)
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return err
	}
	m["pkg"] = pkg
	f.mu.Lock()
	f.msgs = append(f.msgs, m)
	b := f.to[pkg]
	f.mu.Unlock()
	if b != nil {
		var x any
		_ = json.Unmarshal(raw, &x)
		b.recv(x)
	}
	return nil
}

func (f *fakeBand) wait(t *testing.T, ok func(m map[string]any) bool) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		msgs := append([]map[string]any{}, f.msgs...)
		f.mu.Unlock()
		for _, m := range msgs {
			if ok(m) {
				return m
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("等不到想要的消息")
	return nil
}

func (f *fakeBand) all(typ string) []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []map[string]any
	for _, m := range f.msgs {
		if m["t"] == typ {
			out = append(out, m)
		}
	}
	return out
}

func startHost(t *testing.T, builtin fs.FS, userDir string) (*Host, *fakeBand) {
	t.Helper()
	f := &fakeBand{to: map[string]*bandSim{}}
	h := NewHost()
	h.Send = f.send
	h.Load(builtin, userDir)
	ctx, cancel := context.WithCancel(context.Background())
	go h.Run(ctx)
	t.Cleanup(cancel)
	return h, f
}

// ---------- 模拟的手环：在 goja 里跑编好的安装包（app.js + 页面），框架的部分自己模拟 ----------

const bandHarness = `
var global = this;
var conn = { send: function (o) { __send(JSON.stringify(o.data)); if (o.success) o.success(); } };
var storageData = {};
var $app_require$ = function (name) {
  switch (name) {
    case '@app-module/system.interconnect': return { instance: function () { return conn; } };
    case '@app-module/system.storage': return {
      get: function (o) { if (o.key in storageData) { if (o.success) o.success(storageData[o.key]); } else if (o.fail) o.fail(); },
      set: function (o) { storageData[o.key] = o.value; if (o.success) o.success(); }
    };
    case '@app-module/system.device': return { getInfo: function (o) { o.success({ screenWidth: 336, screenHeight: 480 }); } };
    case '@app-module/system.vibrator': return { vibrate: function (o) { __send(JSON.stringify({ t: 'vibrate', mode: o.mode })); } };
  }
  return {};
};
var aiot = {
  __ce__: function (tag, o, ch) { return { tag: tag, opts: o.__opts__ || {}, children: ch || [] }; },
  __ci__: function (o, fn) { return { ci: true, shown: o.__opts__.shown, fn: fn }; },
  __cf__: function (o, fn) { return { cf: true, exp: o.__opts__.exp, fn: fn }; }
};
var vm = null, page = null, handlers = {};
function start() {
  var app = {};
  __app(global, global, global, app);
  var exp = {};
  __page(global, global, global, exp);
  exp.entry(exp);
  page = exp.default;
  vm = {};
  Object.keys(page).forEach(function (k) {
    if (typeof page[k] === 'function' && k !== 'template') vm[k] = page[k].bind(vm);
  });
  Object.assign(vm, JSON.parse(JSON.stringify(page.data || {})));
  vm.onInit();
  vm.onShow();
  if (conn.onopen) conn.onopen();
}
function recv(m) { conn.onmessage({ data: m }); }
function hide() { vm.onHide(); }
// 画一遍：返回元素树（文字、事件编号），事件处理函数按编号记下来，click(编号) 触发
function render() {
  handlers = {};
  var seq = 0;
  var ev = function (v) { return typeof v === 'function' ? v() : v; };
  function expand(n) {
    if (n.ci) return n.shown() ? flat(n.fn()) : [];
    if (n.cf) {
      var e = n.exp();
      var list = e && e.__list__ !== undefined ? e.__list__ : e;
      var out = [];
      (list || []).forEach(function (item, i) { out = out.concat(flat(n.fn(i, item))); });
      return out;
    }
    var o = { tag: n.tag };
    Object.keys(n.opts).forEach(function (k) {
      if (k === 'events') {
        o.ev = 'e' + (++seq);
        o.on = Object.keys(n.opts.events);
        handlers[o.ev] = n.opts.events;
      } else if (k !== 'dataset') o[k] = ev(n.opts[k]);
    });
    o.children = flat(n.children);
    return [o];
  }
  function flat(list) {
    var out = [];
    list.forEach(function (x) { out = out.concat(expand(x)); });
    return out;
  }
  return JSON.stringify(flat([page.template(vm)])[0]);
}
function click(id) { handlers[id].click({ type: 'click' }); }
`

type bandSim struct {
	t    *testing.T
	loop *eventloop.EventLoop
	mu   sync.Mutex
	sent []map[string]any
	// onSend 非空时，app 发给 Mac 的消息交给它（接到 Host 上）
	onSend func(m map[string]any)
}

// runApp 把 app 编好的安装包放进模拟的手环里打开
func runApp(t *testing.T, a *App) *bandSim {
	t.Helper()
	if a.Err != "" {
		t.Fatalf("编译失败：%s", a.Err)
	}
	files := map[string]string{}
	for _, f := range a.Files {
		files[f.Name] = string(f.Body)
	}
	mod := func(js string) string { return "function" + strings.TrimPrefix(js, "export default function") }
	b := &bandSim{t: t, loop: eventloop.NewEventLoop()}
	b.loop.Start()
	t.Cleanup(func() { b.loop.Stop() })
	b.do(func(vm *goja.Runtime) error {
		_ = vm.Set("__send", func(s string) {
			var m map[string]any
			_ = json.Unmarshal([]byte(s), &m)
			b.mu.Lock()
			b.sent = append(b.sent, m)
			fn := b.onSend
			b.mu.Unlock()
			if fn != nil {
				go fn(m)
			}
		})
		if _, err := vm.RunString(bandHarness); err != nil {
			return err
		}
		if _, err := vm.RunString("var __app = " + mod(files["app.js"]) + ";\nvar __page = " + mod(files["pages/index/index.js"])); err != nil {
			return fmt.Errorf("加载页面：%w", err)
		}
		_, err := vm.RunString("start()")
		return err
	})
	return b
}

// do 在事件循环上执行，等它跑完
func (b *bandSim) do(fn func(vm *goja.Runtime) error) {
	b.t.Helper()
	done := make(chan error, 1)
	b.loop.RunOnLoop(func(vm *goja.Runtime) { done <- fn(vm) })
	if err := <-done; err != nil {
		b.t.Fatal(err)
	}
}

func (b *bandSim) run(js string) string {
	b.t.Helper()
	var out string
	b.do(func(vm *goja.Runtime) error {
		v, err := vm.RunString(js)
		if err == nil && v != nil {
			out = v.String()
		}
		return err
	})
	return out
}

// recv：Mac 发来一条消息
func (b *bandSim) recv(m any) {
	b.loop.RunOnLoop(func(vm *goja.Runtime) {
		fn, _ := goja.AssertFunction(vm.Get("recv"))
		if _, err := fn(goja.Undefined(), vm.ToValue(m)); err != nil {
			b.t.Error(err)
		}
	})
}

func (b *bandSim) store(name string, v any) {
	raw, _ := json.Marshal(v)
	var x any
	_ = json.Unmarshal(raw, &x)
	b.recv(map[string]any{"t": "store", "name": name, "v": x})
}

// tree 画一遍
func (b *bandSim) tree() map[string]any {
	var m map[string]any
	_ = json.Unmarshal([]byte(b.run("render()")), &m)
	return m
}

// waitText 等到页面上出现这些文字（按顺序连起来，| 分隔）
func (b *bandSim) waitText(want string) map[string]any {
	b.t.Helper()
	var last map[string]any
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		last = b.tree()
		if strings.Contains(strings.Join(texts(last), "|"), want) {
			return last
		}
	}
	b.t.Fatalf("等不到文字 %q，现在是 %v；发出的消息 %v", want, texts(last), b.messages(""))
	return nil
}

func (b *bandSim) click(id string) { b.run("click(" + jsonString(id) + ")") }

func (b *bandSim) messages(typ string) []map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []map[string]any
	for _, m := range b.sent {
		if typ == "" || m["t"] == typ {
			out = append(out, m)
		}
	}
	return out
}

func jsonString(s string) string { raw, _ := json.Marshal(s); return string(raw) }

func texts(n map[string]any) []string {
	var out []string
	if x, ok := n["value"].(string); ok && (n["tag"] == "text" || n["tag"] == "span") {
		out = append(out, x)
	} else if x, ok := n["value"].(float64); ok && n["tag"] == "text" {
		out = append(out, fmt.Sprint(x))
	}
	if c, ok := n["children"].([]any); ok {
		for _, x := range c {
			out = append(out, texts(x.(map[string]any))...)
		}
	}
	return out
}

// findNode 找第一个满足 pred 的元素
func findNode(n map[string]any, pred func(map[string]any) bool) map[string]any {
	if pred(n) {
		return n
	}
	if c, ok := n["children"].([]any); ok {
		for _, x := range c {
			if r := findNode(x.(map[string]any), pred); r != nil {
				return r
			}
		}
	}
	return nil
}

func hasClass(n map[string]any, c string) bool {
	list, _ := n["classList"].([]any)
	for _, x := range list {
		if x == c {
			return true
		}
	}
	return false
}

func builtinApp(t *testing.T, name string) *App {
	t.Helper()
	sub, err := fs.Sub(apps.FS, name)
	if err != nil {
		t.Fatal(err)
	}
	a, err := compile(sub, "", true)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// ---------- 手环上的 app ----------

func agentList(statuses ...string) []map[string]any {
	names := []string{"api", "web", "cli", "doc"}
	var out []map[string]any
	for i, s := range statuses {
		out = append(out, map[string]any{"id": names[i], "agent": "claude", "project": names[i], "title": "任务" + names[i], "status": s, "seq": i})
	}
	return out
}

func TestAgentCat(t *testing.T) {
	a := builtinApp(t, "agent-cat")
	if a.Package != "dev.wristkit.agent_cat" {
		t.Fatalf("包名 %s", a.Package)
	}
	b := runApp(t, a)
	if len(b.messages("hello")) == 0 {
		t.Fatal("打开时应该发 hello")
	}
	b.waitText("都在休息 zZ")
	// 还不知道选的哪只猫（config 没到）时先不画，不然会先闪一下默认的橘猫
	sprite := findNode(b.tree(), func(n map[string]any) bool { return hasClass(n, "sprite") })
	if frames, _ := sprite["images"].([]any); len(frames) != 0 {
		t.Fatalf("config 没到就画了猫：%v", frames)
	}
	b.store("agents", agentList("blocked", "working", "idle"))
	tree := b.waitText("1 个等你处理！")
	b.waitText("api|1/2|任务api")
	// 点气泡换下一个：卡住的 > 在跑的
	bubble := findNode(tree, func(n map[string]any) bool { return hasClass(n, "bubble") })
	if bubble == nil || bubble["ev"] == nil {
		t.Fatalf("没找到气泡：%v", tree)
	}
	b.click(bubble["ev"].(string))
	b.waitText("web|2/2|任务web")
	// 配置：换猫
	b.store("config", map[string]any{"skin": "gray"})
	sprite = findNode(b.tree(), func(n map[string]any) bool { return hasClass(n, "sprite") })
	frames, _ := sprite["images"].([]any)
	if len(frames) != 6 || frames[0].(map[string]any)["src"] != "/assets/222/gray/alert_0.png" || sprite["duration"] != "467ms" {
		t.Fatalf("猫的动画不对：%v %v", sprite["images"], sprite["duration"])
	}
	// 安装包里有这张图
	found := false
	for _, f := range a.Files {
		found = found || f.Name == "assets/222/gray/alert_0.png"
	}
	if !found {
		t.Fatal("安装包里没有猫的图")
	}
}

// 太大的数据分片发，手环上拼回去
func TestAgentCatParts(t *testing.T) {
	b := runApp(t, builtinApp(t, "agent-cat"))
	raw, _ := json.Marshal(agentList("working", "working"))
	msgs := splitStore("agents", json.RawMessage(strings.Repeat(" ", chunkChars*2)+string(raw)))
	if len(msgs) != 3 {
		t.Fatalf("应该拆成 3 片：%d", len(msgs))
	}
	for i := len(msgs) - 1; i >= 0; i-- { // 倒着到也能拼
		b.recv(roundTrip(msgs[i]))
	}
	b.waitText("2 个在忙")
}

func roundTrip(v any) any {
	raw, _ := json.Marshal(v)
	var x any
	_ = json.Unmarshal(raw, &x)
	return x
}

// 数据存在手环上：下次打开先用它画
func TestCacheAndLog(t *testing.T) {
	b := runApp(t, builtinApp(t, "agent-cat"))
	b.store("agents", agentList("working"))
	b.waitText("1 个在忙")
	b.run("hide()")
	if len(b.messages("bye")) != 1 {
		t.Fatal("离开时应该发 bye")
	}
	cached := b.run("storageData['wristkit:stores']")
	if !strings.Contains(cached, `"status":"working"`) {
		t.Fatalf("没存下数据：%s", cached)
	}
	// 页面方法里抛的异常发回 Mac
	b.store("agents", "不是列表")
	deadline := time.Now().Add(3 * time.Second)
	for len(b.messages("log")) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	logs := b.messages("log")
	if len(logs) == 0 || logs[0]["level"] != "error" || !strings.Contains(logs[0]["msg"].(string), "TypeError") {
		t.Fatalf("应该报出错：%v", logs)
	}
}

// Mac 和手环连起来：手环打开发 hello → Mac 推数据；点一下 → server.js 处理 → 推新数据 → 手环上变了
func TestEndToEnd(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, "hello", map[string]string{
		"manifest.json": `{"name": "hello", "title": "你好"}`,
		"index.ux": `<template>
  <div class="page" @click="ping"><text class="n">{{n}}</text><text>{{label}}</text></div>
</template>
<script>
import { call } from 'wristkit'
export default {
  private: { n: 0, label: '' },
  onStore(name, v) {
    if (name === 'n') this.n = v
    if (name === 'config') this.label = v.label
  },
  ping() { console.log('点了'); call('ping', 21) }
}
</script>
<style>
.page { flex-direction: column; }
</style>`,
		"server.js": `import { on, publish } from 'wristkit/server'
publish('n', 1)
on('ping', (x) => publish('n', x * 2))`,
	})
	h, f := startHost(t, nil, dir)
	a := h.find("hello")
	b := runApp(t, a)
	f.mu.Lock()
	f.to[a.Package] = b
	f.mu.Unlock()
	b.mu.Lock()
	b.onSend = func(m map[string]any) { h.HandleMessage(a.Package, m) }
	b.mu.Unlock()
	// 打开时发的 hello 在接上 Host 之前，再发一次
	b.run("conn.onopen()")
	tree := b.waitText("1")
	b.click(tree["ev"].(string))
	b.waitText("42")
	f.wait(t, func(m map[string]any) bool { return m["pkg"] == a.Package && m["name"] == "n" })
	if h.Foreground() != "hello" {
		t.Fatalf("手环上开着的应该是 hello：%q", h.Foreground())
	}
	// 没开着时不推，打开时补上
	b.run("hide()")
	deadline := time.Now().Add(2 * time.Second)
	for h.Foreground() != "" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	n := len(f.all("store"))
	_ = h.Call("hello", "ping", []any{5.0})
	time.Sleep(100 * time.Millisecond)
	if len(f.all("store")) != n {
		t.Fatal("app 没开着时不该推")
	}
	b.run("vm.onShow()")
	b.waitText("10")
}

func TestUserAppErrorsAndReload(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, "hello", map[string]string{
		"manifest.json": `{"name": "hello", "title": "你好"}`,
		"index.ux":      `<template><div><text>{{a +}}</text></div></template>`,
	})
	h, _ := startHost(t, nil, dir)
	if info := h.Apps(); len(info) != 1 || !strings.Contains(info[0].Error, "编译失败") || info[0].BuildError == "" {
		t.Fatalf("界面上应该看到编译错误：%v", info)
	}
	v0 := h.Apps()[0].Version
	writeFiles(t, dir, "hello", map[string]string{"index.ux": `<template><div><text>好了</text></div></template>`})
	deadline := time.Now().Add(5 * time.Second)
	for h.Apps()[0].Error != "" && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	info := h.Apps()[0]
	if info.Error != "" || info.Version == v0 || info.Package != "dev.wristkit.hello" {
		t.Fatalf("改好后应该重新编译：%+v", info)
	}
	if _, _, err := h.Build("hello"); err != nil {
		t.Fatal(err)
	}
	// 手环上报的运行错误显示在界面上
	h.HandleMessage(info.Package, map[string]any{"t": "log", "level": "error", "msg": "boom"})
	if info := h.Apps()[0]; info.Error != "boom" || info.BuildError != "" {
		t.Fatalf("运行错误没记下来，或者被当成了编译错误（会挡住安装）：%+v", info)
	}
	// 装了新版本，旧版本报的错就清掉
	h.ClearRunErr("hello")
	if e := h.Apps()[0].Error; e != "" {
		t.Fatalf("运行错误没清掉：%s", e)
	}
}

func TestPackageName(t *testing.T) {
	for in, want := range map[string]string{"agent-cat": "dev.wristkit.agent_cat", "Hello2": "dev.wristkit.hello2", "9x": "dev.wristkit.a9x"} {
		if got := packageName(in); got != want {
			t.Errorf("%s → %s，应该是 %s", in, got, want)
		}
	}
}

func TestServerRejectsOtherImports(t *testing.T) {
	fsys := fstest.MapFS{"server.js": {Data: []byte(`import fs from 'fs'`)}}
	if _, err := buildFor(fsys, "server.js", "miband/server"); err == nil || !strings.Contains(err.Error(), "只能 import") {
		t.Fatalf("应该拒绝 import 'fs'：%v", err)
	}
}

// ---------- server.js ----------

func writeFiles(t *testing.T, dir, name string, files map[string]string) {
	t.Helper()
	for f, content := range files {
		p := filepath.Join(dir, name, f)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMacJS(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, "hello", map[string]string{
		"manifest.json": `{"name": "hello"}`,
		"index.ux":      `<template><div></div></template>`,
		"server.js": `import { on, publish, getStore, onStore, storage } from 'miband/server'
on('ping', (n) => {
  storage.set('count', (storage.get('count') || 0) + 1)
  publish('pong', { n: n * 2, count: storage.get('count'), band: getStore('band').connected, other: getStore('other:secret') === undefined })
})
onStore('band', (band) => publish('seen', band.foreground))`,
	})
	h, f := startHost(t, nil, dir)
	h.DataDir = t.TempDir()
	h.SetStore("band", map[string]any{"connected": true, "foreground": false})
	h.SetStore("other:secret", "别的插件的数据")
	h.HandleMessage(packageName("hello"), map[string]any{"t": "hello"})
	// 手环上的插件 call('ping', 21)
	h.HandleMessage(packageName("hello"), map[string]any{"t": "call", "name": "ping", "args": []any{21.0}})
	m := f.wait(t, func(m map[string]any) bool { return m["t"] == "store" && m["name"] == "pong" })
	if v := m["v"].(map[string]any); v["n"] != 42.0 || v["count"] != 1.0 || v["band"] != true || v["other"] != true {
		t.Fatalf("pong 不对：%v", v)
	}
	h.SetStore("band", map[string]any{"connected": true, "foreground": true})
	f.wait(t, func(m map[string]any) bool { return m["t"] == "store" && m["name"] == "seen" && m["v"] == true })
	if info := h.Apps(); info[0].Error != "" || !info[0].HasServer || info[0].NeedGrant {
		t.Fatalf("状态不对：%+v", info[0])
	}
}

func TestMacJSPermissions(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(t.TempDir(), "secret.txt")
	_ = os.WriteFile(secret, []byte("机密"), 0o644)
	allowedDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(allowedDir, "ok.txt"), []byte("可以读"), 0o644)
	writeFiles(t, dir, "reader", map[string]string{
		"manifest.json": fmt.Sprintf(`{"name": "reader", "permissions": {"read": [%q]}}`, allowedDir),
		"index.ux":      `<template><div></div></template>`,
		"server.js": fmt.Sprintf(`import { on, publish, readFile, exec } from 'miband/server'
on('read', () => publish('ok', readFile(%q)))
on('steal', () => publish('stolen', readFile(%q)))
on('run', () => exec('echo', ['hi']))`, filepath.Join(allowedDir, "ok.txt"), secret),
	})
	h, f := startHost(t, nil, dir)
	h.HandleMessage(packageName("reader"), map[string]any{"t": "hello"})

	// 要了权限、还没允许：server.js 不跑
	info := h.Apps()[0]
	if !info.NeedGrant || strings.Join(info.Permissions, ",") != "读 "+allowedDir {
		t.Fatalf("应该等用户允许：%+v", info)
	}
	if err := h.Call("reader", "read", nil); err == nil {
		t.Fatal("没允许时 server.js 不该跑")
	}

	// 允许后：能读允许的目录，读别处、执行命令都不行
	h.SetGrants(map[string]string{"reader": info.PermKey})
	if h.Apps()[0].NeedGrant {
		t.Fatal("已经允许了")
	}
	_ = h.Call("reader", "read", nil)
	f.wait(t, func(m map[string]any) bool {
		return m["t"] == "store" && m["name"] == "ok" && m["v"] == "可以读"
	})
	_ = h.Call("reader", "steal", nil)
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(h.Apps()[0].Error, "没有读") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if e := h.Apps()[0].Error; !strings.Contains(e, "没有读") {
		t.Fatalf("读别处应该报没权限：%q", e)
	}
	if len(f.all("store")) > 0 {
		for _, m := range f.all("store") {
			if m["name"] == "stolen" {
				t.Fatal("不该读到")
			}
		}
	}
}

func TestAgentCatUsage(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", t.TempDir()) // 没有 herdr
	writeFiles(t, home, "Library/Application Support/miband-pet", map[string]string{
		"claude-status.json": `{"rate_limits":{"five_hour":{"used_percentage":63,"resets_at":4000000000},"seven_day":{"used_percentage":41,"resets_at":1000}}}`,
	})
	codex := filepath.Join(home, ".codex", "sessions", "2026", "10")
	writeFiles(t, codex, "20", map[string]string{"rollout-a.jsonl": `{"payload":{"rate_limits":{"primary":{"used_percent":99,"window_minutes":10080,"resets_at":4000000000}}}}` + "\n"})
	time.Sleep(20 * time.Millisecond)
	writeFiles(t, codex, "21", map[string]string{"rollout-b.jsonl": strings.Repeat("x", 100) + "\n" +
		`{"timestamp":"2026-10-21T09:00:00Z","payload":{"rate_limits":{"primary":{"used_percent":10,"window_minutes":300,"resets_at":4000000000}}}}` + "\n" +
		`{"timestamp":"2026-10-21T09:47:18.099Z","payload":{"type":"token_count","rate_limits":{"primary":{"used_percent":20.0,"window_minutes":10080,"resets_at":4000000000},"secondary":null}}}` + "\n" +
		`{"timestamp":"2026-10-21T09:48:00Z","payload":{"type":"agent_message"}}` + "\n"})

	h, f := startHost(t, apps.FS, "")
	h.HandleMessage(catPkg, map[string]any{"t": "hello"})
	m := f.wait(t, func(m map[string]any) bool { return m["t"] == "store" && m["name"] == "usage" })
	raw, _ := json.Marshal(m["v"])
	want := `[{"agent":"claude","updated":`
	if !strings.HasPrefix(string(raw), want) || !strings.Contains(string(raw), `"windows":[{"name":"5h","resetsAt":4000000000,"used":63}]`) ||
		!strings.Contains(string(raw), `{"agent":"codex","updated":1792576038,"windows":[{"name":"7d","resetsAt":4000000000,"used":20}]}`) {
		t.Fatalf("额度不对：%s", raw)
	}
	if e := h.Apps()[0].Error; e != "" {
		t.Fatalf("猫出错：%s", e)
	}
}

func TestAppSettings(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // 没有 herdr
	h, f := startHost(t, apps.FS, "")
	h.HandleMessage(catPkg, map[string]any{"t": "hello"})
	// 没改过：推默认值
	f.wait(t, func(m map[string]any) bool {
		v, _ := m["v"].(map[string]any)
		return m["t"] == "store" && m["name"] == "config" && v["quota"] == "auto"
	})
	if err := h.SetConfig("agent-cat", "quota", "gemini"); err == nil {
		t.Fatal("不在选项里的值应该拒绝")
	}
	if err := h.SetConfig("agent-cat", "nope", "x"); err == nil {
		t.Fatal("没有的配置项应该拒绝")
	}
	if err := h.SetConfig("agent-cat", "quota", "claude:7d"); err != nil {
		t.Fatal(err)
	}
	f.wait(t, func(m map[string]any) bool {
		v, _ := m["v"].(map[string]any)
		return m["t"] == "store" && m["name"] == "config" && v["quota"] == "claude:7d" && v["skin"] == "orange"
	})
	info := h.Apps()[0]
	if len(info.Settings) != 7 || info.Config["quota"] != "claude:7d" || info.Config["alert"] != "notify" {
		t.Fatalf("界面上的配置不对：%+v %+v", info.Settings, info.Config)
	}
}

// Agent 猫在 Mac 上读 herdr：推 agent 状态，数今天完成了几个，跑完、卡住时提醒
func TestAgentCatHerdr(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// 假的 herdr：输出 agents.json
	bin, list := t.TempDir(), filepath.Join(t.TempDir(), "agents.json")
	t.Setenv("PATH", bin)
	writeFiles(t, bin, "", map[string]string{"herdr": "#!/bin/sh\n/bin/cat " + list + "\n"})
	_ = os.Chmod(filepath.Join(bin, "herdr"), 0o755)
	setAgents := func(statuses ...string) {
		var agents []map[string]any
		for i, s := range statuses {
			agents = append(agents, map[string]any{"pane_id": fmt.Sprintf("p%d", i), "agent": "claude", "agent_status": s,
				"cwd": "/work/api/", "name": "", "terminal_title_stripped": fmt.Sprintf("任务%d", i)})
		}
		raw, _ := json.Marshal(map[string]any{"result": map[string]any{"agents": agents}})
		if err := os.WriteFile(list, raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// 旧版 App 记的今天完成数，第一次跑时搬过来
	today := time.Now().Format("2006-01-02")
	writeFiles(t, home, "Library/Application Support/miband-pet", map[string]string{
		"stats.json": fmt.Sprintf(`{"days":{%q:{"done":4,"fed":1}},"fishLeft":3}`, today),
	})
	setAgents("working", "idle")

	var mu sync.Mutex
	var notes, wakes []string
	f := &fakeBand{}
	h := NewHost()
	h.Send = f.send
	h.DataDir = t.TempDir()
	h.NotifyBand = func(title, body string) error {
		mu.Lock()
		notes = append(notes, title+"|"+body)
		mu.Unlock()
		return nil
	}
	h.Wake = func(app string) error {
		mu.Lock()
		wakes = append(wakes, app)
		mu.Unlock()
		return nil
	}
	h.Load(apps.FS, "")
	ctx, cancel := context.WithCancel(context.Background())
	go h.Run(ctx)
	t.Cleanup(cancel)
	h.HandleMessage(catPkg, map[string]any{"t": "hello"})
	storeIs := func(name, want string) func(m map[string]any) bool {
		return func(m map[string]any) bool {
			raw, _ := json.Marshal(m["v"])
			return m["t"] == "store" && m["name"] == name && strings.Contains(string(raw), want)
		}
	}
	f.wait(t, storeIs("agents", `{"agent":"claude","id":"p0","project":"api","seq":0,"status":"working","title":"任务0"}`))
	f.wait(t, storeIs("stats", `"today":{"done":4}`))

	// 跑完一个：今天的数 +1，发通知
	setAgents("done", "idle")
	f.wait(t, storeIs("stats", `"today":{"done":5}`))
	waitFor := func(what string, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) {
			mu.Lock()
			done := ok()
			mu.Unlock()
			if done {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("等不到%s：通知 %v，亮屏 %v", what, notes, wakes)
	}
	waitFor("跑完的通知", func() bool { return len(notes) == 1 && notes[0] == "claude 完成|api · 任务0" })

	// 改成只亮屏：卡住时点亮手环、切到猫，不发通知
	if err := h.SetConfig("agent-cat", "alert", "wake"); err != nil {
		t.Fatal(err)
	}
	setAgents("blocked", "idle")
	waitFor("亮屏", func() bool { return len(wakes) == 1 && wakes[0] == "agent-cat" && len(notes) == 1 })
	if e := h.Apps()[0].Error; e != "" {
		t.Fatalf("猫出错：%s", e)
	}
}

// Agent 猫记 Claude Code 每天花了多少钱：按会话看状态栏数据里的累计钱数，涨了多少记多少
func TestAgentCatCost(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", t.TempDir()) // 没有 herdr
	dir := filepath.Join(home, "Library/Application Support/miband-pet")
	_ = os.MkdirAll(dir, 0o755)
	file := filepath.Join(dir, "claude-status.json")
	write := func(session string, usd float64, startedAgo time.Duration, at time.Time) {
		raw := fmt.Sprintf(`{"session_id":%q,"cost":{"total_cost_usd":%v,"total_duration_ms":%d}}`, session, usd, startedAgo.Milliseconds())
		if err := os.WriteFile(file, []byte(raw), 0o644); err != nil {
			t.Fatal(err)
		}
		_ = os.Chtimes(file, at, at) // 每次写的修改时间不一样，插件才知道更新了
	}
	now := time.Now()
	// 开始记账之前就开着、已经花了 50 的会话：之前的不算
	write("old", 50, 2*time.Hour, now)

	h, f := startHost(t, apps.FS, "")
	h.DataDir = t.TempDir()
	h.HandleMessage(catPkg, map[string]any{"t": "hello"})
	// 一打开就推一次
	f.wait(t, func(m map[string]any) bool {
		raw, _ := json.Marshal(m["v"])
		return m["t"] == "store" && m["name"] == "cost" && strings.Contains(string(raw), `"today":0`)
	})
	// 之后推给手环最多 30 秒一次，这里看插件存下来的账
	costIs := func(want float64) {
		t.Helper()
		key := now.Format("2006-01-02")
		var got any
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
			raw, _ := os.ReadFile(filepath.Join(h.DataDir, "agent-cat.json"))
			var data struct {
				Cost struct{ Days map[string]float64 } `json:"cost"`
			}
			_ = json.Unmarshal(raw, &data)
			if got = data.Cost.Days[key]; got == want {
				return
			}
		}
		t.Fatalf("今天的钱数是 %v，想要 %v", got, want)
	}
	write("old", 52.5, 2*time.Hour, now.Add(time.Second)) // 涨了 2.5
	costIs(2.5)
	write("new", 1.25, time.Second, now.Add(2*time.Second)) // 记账之后开的会话：全算
	costIs(3.75)
	write("old", 0.5, time.Second, now.Add(3*time.Second)) // 会话重新打开，钱数从 0 重算
	costIs(4.25)

	// 新版状态栏脚本按会话各存一份：几个会话同时刷新也都记得到（这时不再看 claude-status.json）
	sessions := filepath.Join(dir, "claude-sessions")
	_ = os.MkdirAll(sessions, 0o755)
	writeSession := func(session string, usd float64, at time.Time) {
		f := filepath.Join(sessions, session+".json")
		raw := fmt.Sprintf(`{"session_id":%q,"cost":{"total_cost_usd":%v,"total_duration_ms":1000}}`, session, usd)
		if err := os.WriteFile(f, []byte(raw), 0o644); err != nil {
			t.Fatal(err)
		}
		_ = os.Chtimes(f, at, at)
	}
	writeSession("old", 0.75, now.Add(4*time.Second)) // +0.25
	writeSession("x", 2, now.Add(4*time.Second))      // 新会话 +2
	writeSession("y", 3, now.Add(4*time.Second))      // 同时刷新的另一个新会话 +3
	costIs(9.5)
	if e := h.Apps()[0].Error; e != "" {
		t.Fatalf("猫出错：%s", e)
	}

	// 手环上：猫页小鱼干旁边、今日页都显示钱数；点钱数柱状图换成钱
	b := runApp(t, builtinApp(t, "agent-cat"))
	days := []map[string]any{}
	for i, v := range []float64{0, 3, 12.5, 0, 1, 0, 4.25} {
		days = append(days, map[string]any{"date": fmt.Sprintf("10-%02d", 17+i), "weekday": "一二三四五六日"[i*3 : i*3+3], "usd": v})
	}
	b.store("cost", map[string]any{"today": 4.25, "days": days})
	b.waitText("$4.3")
	tree := b.waitText("$4.25|Claude 花的钱")
	usd := findNode(tree, func(n map[string]any) bool {
		on, _ := n["on"].([]any)
		return len(on) > 0 && on[0] == "click" && strings.Contains(strings.Join(texts(n), "|"), "Claude 花的钱")
	})
	if usd == nil {
		t.Fatal("找不到钱数那一块")
	}
	b.click(usd["ev"].(string))
	b.waitText("$13")
	b.waitText("$3.0")
}
