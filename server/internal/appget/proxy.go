package appget

import (
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"time"
)

// NewHTTPClient 走代理：先看环境变量（HTTPS_PROXY 这些），没有就用 macOS 系统设置里的 HTTPS / HTTP / SOCKS 代理。
// 从访达打开的 App 拿不到终端里设的环境变量，在国内连 GitHub 多半靠系统代理。自动代理配置（PAC）不认。
func NewHTTPClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	sys := systemProxy()
	tr.Proxy = func(r *http.Request) (*url.URL, error) {
		if u, err := http.ProxyFromEnvironment(r); u != nil || err != nil {
			return u, err
		}
		return sys, nil
	}
	return &http.Client{Transport: tr, Timeout: 30 * time.Second}
}

// systemProxy 读 scutil --proxy；没开代理、读不到返回 nil
func systemProxy() *url.URL {
	out, err := exec.Command("scutil", "--proxy").Output()
	if err != nil {
		return nil
	}
	return parseScutil(string(out))
}

func parseScutil(out string) *url.URL {
	kv := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(line, " : "); ok {
			kv[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	for _, p := range []struct{ key, scheme string }{{"HTTPS", "http"}, {"HTTP", "http"}, {"SOCKS", "socks5"}} {
		if kv[p.key+"Enable"] == "1" && kv[p.key+"Proxy"] != "" && kv[p.key+"Port"] != "" {
			return &url.URL{Scheme: p.scheme, Host: kv[p.key+"Proxy"] + ":" + kv[p.key+"Port"]}
		}
	}
	return nil
}
