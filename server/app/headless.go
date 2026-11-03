package main

// 后台运行：不开窗口、不进菜单栏，连手环、跑 app 在 Mac 上的部分，和 App 一样；设置页由本机 HTTP 提供，
// 用浏览器打开。给 gerdr 当插件就是这么跑的（gerdr 起这个进程、它退出了再拉起来）：
//
//	wristkit serve [-addr 127.0.0.1:7858]   在终端里跑
//	wristkit gerdr [-addr 127.0.0.1:7858]   gerdr 插件的服务进程：stdin / stdout 按 gerdr 的 JSON 行协议说话，
//	                                         stdin 关掉就断开手环退出
//
// 设置页就是 App 里那个前端（frontend/dist），原样拿来用：页面里的 window.go.main.App.* 和 window.runtime.*
// 由 /wristkit-shim.js 换成 HTTP（POST /api/call/<方法>）和事件流（GET /api/events），前端代码不用分两套。

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"time"
)

const defaultAddr = "127.0.0.1:7858"

func runHeadless(mode string, args []string) int {
	fset := flag.NewFlagSet(mode, flag.ExitOnError)
	addr := fset.String("addr", defaultAddr, "设置页的 HTTP 地址（只听本机）")
	fset.Parse(args)

	// 先确认端口能用：被占了（比如已经有一个在跑）就别去抢手环
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		slog.Error("设置页的端口被占用", "addr", *addr, "err", err)
		if mode == "gerdr" {
			newGerdrOut().status("端口 " + *addr + " 被占用，是不是已经有一个 wristkit 在跑？")
			time.Sleep(time.Second) // 让 gerdr 读到这行再退出
		}
		return 1
	}
	url := "http://" + ln.Addr().String()

	hub := newEventHub()
	a := NewApp()
	a.headless = true
	a.ctx = context.Background()

	var out *gerdrOut
	if mode == "gerdr" {
		out = newGerdrOut()
		out.url = url
	}
	a.emitFn = func(name string, data any) {
		hub.emit(name, data)
		if name == "state" && out != nil {
			out.state(a.state(), url)
		}
	}

	srv := &http.Server{Handler: newWebHandler(a, hub, ln.Addr().String()), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("设置页的 HTTP 服务出错", "err", err)
		}
	}()
	slog.Info("后台运行", "mode", mode, "settings", url)

	a.bootstrapConfig()
	a.restartService()

	stop := make(chan string, 1)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	go func() { stop <- (<-sig).String() }()
	if out != nil {
		out.state(a.state(), url)
		go func() {
			readGerdr(os.Stdin, func(msg json.RawMessage) { out.reply(msg, a.state(), url) })
			stop <- "stdin 关闭"
		}()
	}

	why := <-stop
	slog.Info("退出", "why", why)
	// 一定要先关掉蓝牙通道再退出，否则手环那边的会话会卡住；gerdr 只等 3 秒
	done := make(chan struct{})
	go func() { a.stopService(); close(done) }()
	select {
	case <-done:
	case <-time.After(2500 * time.Millisecond):
		slog.Warn("等服务退出超时")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	srv.Shutdown(ctx)
	return 0
}

// ---------- gerdr 的 JSON 行协议 ----------

type gerdrOut struct {
	url      string // 设置页地址：每行状态都带上，填进插件的「控制面板」设置项（type link，key panel）的按钮
	mu       sync.Mutex
	enc      *json.Encoder
	last     string
	lastInfo string
}

func newGerdrOut() *gerdrOut { return &gerdrOut{enc: json.NewEncoder(os.Stdout)} }

func (o *gerdrOut) write(v any) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.enc.Encode(v)
}

// status 是 gerdr 设置页里插件下面那行字。
func (o *gerdrOut) status(text string) {
	o.mu.Lock()
	same := o.last == text
	o.last = text
	o.mu.Unlock()
	if !same {
		o.write(map[string]any{"type": "status", "text": text, "links": map[string]string{"panel": o.url}})
	}
}

// info 给插件的页面：设置页地址和手环状态。
func infoOf(st AppState, url string) map[string]any {
	m := map[string]any{"type": "info", "url": url, "configured": st.Configured, "connected": st.Connected, "text": trayText(st)}
	if st.Battery != nil {
		m["battery"] = st.Battery.Level
		m["charging"] = st.Battery.Charging
	}
	return m
}

// state 在手环状态变了时告诉 gerdr；没变的不发（装 app 时进度一秒变好几次）。
func (o *gerdrOut) state(st AppState, url string) {
	o.status(trayText(st))
	info := infoOf(st, url)
	raw, _ := json.Marshal(info)
	o.mu.Lock()
	same := o.lastInfo == string(raw)
	o.lastInfo = string(raw)
	o.mu.Unlock()
	if !same {
		o.write(map[string]any{"type": "message", "data": info})
	}
}

// reply 回插件页面发来的消息：现在只有 {"type":"info"}（页面刚打开，要一份当前状态）。
func (o *gerdrOut) reply(msg json.RawMessage, st AppState, url string) {
	var m struct {
		Type string `json:"type"`
	}
	json.Unmarshal(msg, &m)
	if m.Type == "info" {
		o.write(map[string]any{"type": "message", "data": infoOf(st, url)})
	}
}

// readGerdr 读 gerdr 发来的行，直到 stdin 关掉。会话列表（"sessions"）用不上：Agent 猫自己跑 gerdr session list。
func readGerdr(r io.Reader, onMessage func(json.RawMessage)) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		var line struct {
			Type string          `json:"type"`
			Data json.RawMessage `json:"data"`
		}
		if json.Unmarshal(sc.Bytes(), &line) != nil {
			continue
		}
		if line.Type == "message" {
			onMessage(line.Data)
		}
	}
}

// ---------- 事件流 ----------

type eventHub struct {
	mu   sync.Mutex
	subs map[chan []byte]struct{}
}

func newEventHub() *eventHub { return &eventHub{subs: map[chan []byte]struct{}{}} }

func (h *eventHub) emit(name string, data any) {
	raw, err := json.Marshal(map[string]any{"name": name, "data": data})
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- raw:
		default: // 页面卡住了就丢：state / stores 只是「重新拉一下」的提示
		}
	}
}

func (h *eventHub) subscribe() (chan []byte, func()) {
	ch := make(chan []byte, 64)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.subs, ch)
		h.mu.Unlock()
	}
}

// ---------- HTTP ----------

func newWebHandler(a *App, hub *eventHub, hostport string) http.Handler {
	dist, err := fs.Sub(assets, "frontend/dist")
	if err != nil {
		panic(err)
	}
	_, port, _ := net.SplitHostPort(hostport)
	files := http.FileServerFS(dist)
	mux := http.NewServeMux()

	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		raw, err := fs.ReadFile(dist, "index.html")
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		// 普通 script 在 module 脚本之前执行：前端的代码跑起来时 window.go / window.runtime 已经在了
		html := strings.Replace(string(raw), "<head>", `<head><script src="/wristkit-shim.js"></script>`, 1)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		io.WriteString(w, html)
	})
	mux.HandleFunc("GET /wristkit-shim.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		io.WriteString(w, shimJS)
	})
	mux.HandleFunc("POST /api/call/{method}", func(w http.ResponseWriter, r *http.Request) {
		res, err := callMethod(a, r.PathValue("method"), r.Body)
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"result": res})
	})
	mux.HandleFunc("GET /api/events", func(w http.ResponseWriter, r *http.Request) {
		fl, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "不支持事件流", 500)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		ch, off := hub.subscribe()
		defer off()
		io.WriteString(w, ": hi\n\n")
		fl.Flush()
		ping := time.NewTicker(25 * time.Second)
		defer ping.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case raw := <-ch:
				fmt.Fprintf(w, "data: %s\n\n", raw)
				fl.Flush()
			case <-ping.C:
				io.WriteString(w, ": ping\n\n")
				fl.Flush()
			}
		}
	})
	mux.Handle("GET /", files)

	// 只给本机的页面用：Host 不对（DNS rebinding）、别的网站发来的请求（CSRF）一律拒绝。
	// POST 还要带 X-Wristkit 头：跨站的页面带不了自定义头（要预检，这里不答应）。
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, p, _ := net.SplitHostPort(r.Host)
		if p != port || (host != "127.0.0.1" && host != "localhost") {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if site := r.Header.Get("Sec-Fetch-Site"); site == "cross-site" || site == "same-site" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet && r.Header.Get("X-Wristkit") != "1" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

var errorType = reflect.TypeFor[error]()

// callMethod 按名字调 App 的导出方法，和 Wails 绑定的是同一批方法；参数是 JSON 数组，按方法的参数类型解码。
func callMethod(a *App, name string, body io.Reader) (any, error) {
	m := reflect.ValueOf(a).MethodByName(name)
	if !m.IsValid() || name == "" || name[0] < 'A' || name[0] > 'Z' {
		return nil, fmt.Errorf("没有方法 %s", name)
	}
	var raw []json.RawMessage
	if err := json.NewDecoder(io.LimitReader(body, 32<<20)).Decode(&raw); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	t := m.Type()
	if len(raw) != t.NumIn() {
		return nil, fmt.Errorf("%s 要 %d 个参数，给了 %d 个", name, t.NumIn(), len(raw))
	}
	in := make([]reflect.Value, len(raw))
	for i, r := range raw {
		v := reflect.New(t.In(i))
		if err := json.Unmarshal(r, v.Interface()); err != nil {
			return nil, fmt.Errorf("%s 的第 %d 个参数：%w", name, i+1, err)
		}
		in[i] = v.Elem()
	}
	var res any
	for _, v := range m.Call(in) {
		if v.Type() == errorType {
			if !v.IsNil() {
				return nil, v.Interface().(error)
			}
			continue
		}
		res = v.Interface()
	}
	return res, nil
}

// shimJS 在浏览器里顶替 Wails 注入的 window.go 和 window.runtime。
const shimJS = `(() => {
  const call = async (name, args) => {
    const r = await fetch('/api/call/' + name, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', 'X-Wristkit': '1' },
      body: JSON.stringify(args),
    });
    let j = {};
    try { j = await r.json(); } catch (e) {}
    if (!r.ok) throw (j.error || ('wristkit 没有响应（' + r.status + '）'));
    return j.result;
  };
  window.go = { main: { App: new Proxy({}, { get: (_, name) => (...args) => call(String(name), args) }) } };

  const listeners = {};
  const fire = (name, data) => (listeners[name] || []).slice().forEach((l) => l.cb(data));
  let opened = false;
  const connect = () => {
    const es = new EventSource('/api/events');
    es.onopen = () => {
      // 断开过（wristkit 重启了）：让页面把状态重新拉一遍
      if (opened) { fire('state'); fire('stores'); }
      opened = true;
    };
    es.onmessage = (e) => { try { const m = JSON.parse(e.data); fire(m.name, m.data); } catch (err) {} };
  };
  connect();

  const runtime = {
    EventsOnMultiple(name, cb, max) {
      const l = { cb: (data) => { if (max > 0 && --l.left < 0) return; cb(data); if (max > 0 && l.left === 0) off(); }, left: max };
      const off = () => { listeners[name] = (listeners[name] || []).filter((x) => x !== l); };
      (listeners[name] = listeners[name] || []).push(l);
      return off;
    },
    EventsOff(...names) { names.forEach((n) => delete listeners[n]); },
    EventsOffAll() { for (const n in listeners) delete listeners[n]; },
    EventsEmit(name, ...data) { fire(name, data[0]); },
    BrowserOpenURL(url) { window.open(url, '_blank', 'noopener'); },
    WindowReload() { location.reload(); },
    WindowReloadApp() { location.reload(); },
    LogPrint: console.log, LogTrace: console.debug, LogDebug: console.debug, LogInfo: console.info,
    LogWarning: console.warn, LogError: console.error, LogFatal: console.error,
  };
  // 窗口相关的（标题、大小、置顶……）在浏览器里没有意义，一律不做事
  window.runtime = new Proxy(runtime, { get: (t, k) => (k in t ? t[k] : () => {}) });
})();
`
