package appget

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	cases := map[string]Source{
		"https://github.com/zbysir/wristkit/tree/main/server/apps/tiger-cat": {"zbysir", "wristkit", "main", "server/apps/tiger-cat"},
		"github.com/zbysir/wristkit/tree/dev/apps/x/":                        {"zbysir", "wristkit", "dev", "apps/x"},
		"https://github.com/a/b/blob/main/x/manifest.json":                   {"a", "b", "main", "x"},
		"https://github.com/someone/band-app":                                {"someone", "band-app", "", ""},
		"https://github.com/someone/band-app.git":                            {"someone", "band-app", "", ""},
		"zbysir/wristkit/server/apps/tiger-cat":                              {"zbysir", "wristkit", "", "server/apps/tiger-cat"},
		" zbysir/wristkit/apps/x@feature/new ":                               {"zbysir", "wristkit", "feature/new", "apps/x"},
	}
	for in, want := range cases {
		got, err := Parse(in)
		if err != nil || got != want {
			t.Errorf("Parse(%q) = %+v, %v；要 %+v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "wristkit", "https://gitlab.com/a/b", "a/b/../c", "a/b/c d"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) 应该报错", bad)
		}
	}
	if u := (Source{"a", "b", "", "x/y"}).URL(); u != "https://github.com/a/b/tree/HEAD/x/y" {
		t.Error(u)
	}
}

// fakeGitHub：仓库 a/b 里有 apps/cat（一个 app）和 apps/nope（不是 app）；commit 可以改
type fakeGitHub struct {
	commit string
	files  map[string]string // 仓库里的路径 → 内容
	calls  []string
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.calls = append(f.calls, r.URL.Path)
	switch {
	case r.URL.Path == "/api/repos/a/b/commits":
		p := r.URL.Query().Get("path")
		found := false
		for k := range f.files {
			found = found || strings.HasPrefix(k, p+"/")
		}
		if !found {
			_ = json.NewEncoder(w).Encode([]any{})
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]string{{"sha": f.commit}})
	case r.URL.Path == "/api/repos/a/b/git/trees/"+f.commit:
		var tree []map[string]any
		for k, v := range f.files {
			tree = append(tree, map[string]any{"path": k, "type": "blob", "size": len(v)})
		}
		tree = append(tree, map[string]any{"path": "apps/cat/assets", "type": "tree"})
		_ = json.NewEncoder(w).Encode(map[string]any{"tree": tree})
	case strings.HasPrefix(r.URL.Path, "/raw/a/b/"+f.commit+"/"):
		body, ok := f.files[strings.TrimPrefix(r.URL.Path, "/raw/a/b/"+f.commit+"/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	default:
		http.NotFound(w, r)
	}
}

func TestInstall(t *testing.T) {
	gh := &fakeGitHub{commit: "c1", files: map[string]string{
		"apps/cat/manifest.json":       `{"name": "cat", "title": "猫"}`,
		"apps/cat/index.ux":            "<template><div></div></template>",
		"apps/cat/assets/a 1.png":      "png",
		"apps/cat/.github/x.yml":       "隐藏文件不要",
		"apps/nope/readme.md":          "x",
		"apps/bad/manifest.json":       `{"name": "Bad Name"}`,
		"apps/catalog/manifest.json":   `{"name": "cat"}`, // 和 cat 同名，来源不同
		"apps/other/manifest.json.bak": "",
	}}
	srv := httptest.NewServer(gh)
	defer srv.Close()
	c := &Client{API: srv.URL + "/api", Raw: srv.URL + "/raw"}
	ctx := context.Background()
	dir := t.TempDir()

	res, err := c.Install(ctx, "a/b/apps/cat", dir)
	if err != nil {
		t.Fatal(err)
	}
	if res.Name != "cat" || res.Prev != nil || res.Meta.Commit != "c1" {
		t.Fatalf("%+v", res)
	}
	for _, f := range []string{"manifest.json", "index.ux", "assets/a 1.png", SourceFile} {
		if _, err := os.Stat(filepath.Join(dir, "cat", f)); err != nil {
			t.Errorf("没有 %s", f)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "cat", ".github")); err == nil {
		t.Error("隐藏文件也下了")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("临时目录没清掉：%v", entries)
	}

	// 检查更新、更新
	m, err := ReadMeta(filepath.Join(dir, "cat"))
	if err != nil || m.URL != "https://github.com/a/b/tree/HEAD/apps/cat" {
		t.Fatal(m, err)
	}
	if up, err := c.HasUpdate(ctx, m); err != nil || up {
		t.Fatal("没有更新", up, err)
	}
	gh.commit = "c2"
	gh.files["apps/cat/index.ux"] = "新版"
	delete(gh.files, "apps/cat/assets/a 1.png")
	if up, _ := c.HasUpdate(ctx, m); !up {
		t.Fatal("应该有更新")
	}
	res, err = c.Install(ctx, m.URL, dir)
	if err != nil || res.Prev == nil || res.Prev.Commit != "c1" {
		t.Fatal(res, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "cat", "index.ux")); string(b) != "新版" {
		t.Error("没更新")
	}
	if _, err := os.Stat(filepath.Join(dir, "cat", "assets", "a 1.png")); err == nil {
		t.Error("新版删掉的文件还在")
	}

	// 出错的情况：都不动已有的 app
	for in, want := range map[string]string{
		"a/b/apps/nope":    "没有 manifest.json",
		"a/b/apps/bad":     "name",
		"a/b/apps/catalog": "已经有 cat 了",
		"a/b/apps/missing": "找不到",
		"a/zzz/apps/cat":   "找不到",
	} {
		if _, err := c.Install(ctx, in, dir); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s：%v，要包含 %q", in, err, want)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "cat", "manifest.json")); !strings.Contains(string(b), "猫") {
		t.Error("同名的把原来的盖掉了")
	}
	// 自己放进去的（没有 .source.json）也不覆盖
	_ = os.Remove(filepath.Join(dir, "cat", SourceFile))
	if _, err := c.Install(ctx, "a/b/apps/cat", dir); err == nil {
		t.Error("自己放的 app 不能覆盖")
	}
}

func TestSafeRel(t *testing.T) {
	for p, ok := range map[string]bool{"a.png": true, "assets/a.png": true, "../x": false, "a/../b": false, "/etc/x": false, ".git/config": false, "a//b": false, `a\b`: false} {
		if safeRel(p) != ok {
			t.Errorf("safeRel(%q) 应该是 %v", p, ok)
		}
	}
}

func TestParseScutil(t *testing.T) {
	out := "<dictionary> {\n  HTTPEnable : 0\n  HTTPSEnable : 1\n  HTTPSPort : 7890\n  HTTPSProxy : 127.0.0.1\n  SOCKSEnable : 1\n  SOCKSPort : 7891\n  SOCKSProxy : 127.0.0.1\n}"
	if u := parseScutil(out); u == nil || u.String() != "http://127.0.0.1:7890" {
		t.Fatal(u)
	}
	if u := parseScutil(strings.Replace(out, "HTTPSEnable : 1", "HTTPSEnable : 0", 1)); u == nil || u.String() != "socks5://127.0.0.1:7891" {
		t.Fatal(u)
	}
	if u := parseScutil("<dictionary> {\n  ProxyAutoConfigEnable : 1\n}"); u != nil {
		t.Fatal(u)
	}
}
