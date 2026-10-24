// Package bandapp 是 Mac 这边的 app 宿主。每个 app 在手环上是一个独立的快应用：这里把 app 编译打包成安装包
// （.ux → JS，见 internal/ux；打包签名见 internal/quickapp、internal/rpk），装到手环上；app 打开后和 Mac 通信，
// 收 Mac 推来的数据（数据由 app 的 server.js 在 Mac 上读、推，见 server.go）。
//
// 一个 app 是一个目录：manifest.json + index.ux（页面，可以 import 同目录的 js）+ 图片 + 可选的 server.js。
// 内置 app 打包在 App 里（server/apps），用户 app 放在 ~/Library/Application Support/miband-pet/apps/<名字>/，
// 同名的用户 app 会替换内置 app。写 app 的说明见 docs/apps.md。
package bandapp

import (
	"crypto/sha1"
	_ "embed"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"

	"github.com/evanw/esbuild/pkg/api"

	"miband/internal/quickapp"
	"miband/internal/rpk"
	"miband/internal/ux"
)

// Manifest 是 app 目录下的 manifest.json。
type Manifest struct {
	Name  string `json:"name"`  // 唯一名字，同名的用户 app 替换内置 app
	Title string `json:"title"` // 显示名
	Order int    `json:"order"` // 在 App 列表里的顺序，小的在前；默认 100
	Main  string `json:"main"`  // 页面，默认 index.ux
	// server.js 要用的能力，用户 app 要在 App 里允许后 server.js 才会跑
	Permissions Permissions `json:"permissions"`
	// 配置项：App 照着画设置表单，值推给手环（useConfig()）和 server.js（getConfig()）
	Settings []Setting `json:"settings"`
}

// Setting 是 app 的一个配置项。
type Setting struct {
	Key         string   `json:"key"`
	Title       string   `json:"title"`
	Description string   `json:"description,omitempty"`
	Type        string   `json:"type"`              // select / bool / number / text
	Options     []Option `json:"options,omitempty"` // select 的选项
	Default     any      `json:"default"`
}

type Option struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// check 看 v 是不是这个配置项能接受的值，返回规整后的值
func (s Setting) check(v any) (any, error) {
	switch s.Type {
	case "bool":
		if b, ok := v.(bool); ok {
			return b, nil
		}
	case "number":
		if f, ok := v.(float64); ok {
			return f, nil
		}
	case "text":
		if str, ok := v.(string); ok {
			return str, nil
		}
	case "select":
		if str, ok := v.(string); ok {
			for _, o := range s.Options {
				if o.Value == str {
					return str, nil
				}
			}
		}
	}
	return nil, fmt.Errorf("配置项 %s 不接受 %v", s.Key, v)
}

// Permissions 是 server.js 能做的敏感的事。
type Permissions struct {
	Notify bool     `json:"notify,omitempty"` // 提醒：发通知（Mac 和手环）、点亮手环
	Read   []string `json:"read,omitempty"`   // 能读哪些目录下的文件（可以用 ~）
	Exec   []string `json:"exec,omitempty"`   // 能执行哪些命令（命令名，如 "herdr"）
}

// Empty：什么都没要
func (p Permissions) Empty() bool { return !p.Notify && len(p.Exec) == 0 && len(p.Read) == 0 }

// Key 用来记「用户允许过哪些权限」：权限变了要重新允许
func (p Permissions) Key() string {
	raw, _ := json.Marshal(p)
	return string(raw)
}

// Describe 给界面看的权限说明
func (p Permissions) Describe() []string {
	var out []string
	if p.Notify {
		out = append(out, "发通知、点亮手环")
	}
	for _, r := range p.Read {
		out = append(out, "读 "+r)
	}
	for _, c := range p.Exec {
		out = append(out, "运行 "+c)
	}
	return out
}

// PackagePrefix 是 app 在手环上的包名前缀，后面接 app 的名字
const PackagePrefix = "dev.wristkit."

// App 是编译好的 app。
type App struct {
	Manifest
	Builtin bool
	Dir     string // 用户 app 的目录；内置 app 为空
	Source  string // 从 GitHub 下的 app：来源链接（app 目录里 .source.json 的 url）；不是的为空

	Package    string     // 手环上的包名
	Version    int        // 安装包的版本号：内容变了就变（不一定变大），和手环上装的比，不一样就要更新
	Files      []rpk.File // 安装包里的文件（还没签名）
	Err        string     // 编译出错
	ServerCode string     // 编译好的 server.js；没有就是空
}

func readManifest(fsys fs.FS) (Manifest, error) {
	m := Manifest{Order: 100}
	raw, err := fs.ReadFile(fsys, "manifest.json")
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return m, fmt.Errorf("manifest.json: %w", err)
	}
	if m.Name == "" {
		return m, errors.New("manifest.json 里缺 name")
	}
	if m.Title == "" {
		m.Title = m.Name
	}
	if m.Main == "" {
		m.Main = "index.ux"
	}
	for _, s := range m.Settings {
		switch {
		case s.Key == "":
			return m, errors.New("manifest.json 的 settings 里有一项缺 key")
		case s.Type != "select" && s.Type != "bool" && s.Type != "number" && s.Type != "text":
			return m, fmt.Errorf("配置项 %s 的 type 只能是 select / bool / number / text", s.Key)
		case s.Type == "select" && len(s.Options) == 0:
			return m, fmt.Errorf("配置项 %s 是 select，要有 options", s.Key)
		}
		if _, err := s.check(s.Default); err != nil {
			return m, fmt.Errorf("配置项 %s 的默认值不对", s.Key)
		}
	}
	return m, nil
}

//go:embed wristkit.js
var runtimeJS string

//go:embed logo.png
var defaultIcon []byte

// 页面脚本外面包一层，接上 wristkit 的运行时（连 Mac、收数据）
const pageEntry = `import def from 'ux:script'
import { page } from 'wristkit'
export default page(def || {})
`

// packageName：app 名字 → 包名。包名每段只能是字母、数字、下划线
func packageName(name string) string {
	var b strings.Builder
	for _, c := range strings.ToLower(name) {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			b.WriteRune(c)
		default:
			b.WriteByte('_')
		}
	}
	s := b.String()
	if s == "" || s[0] < 'a' || s[0] > 'z' {
		s = "a" + s
	}
	return PackagePrefix + s
}

// compile 编译 app。编译出错记在 Err 里（App 里显示出来）。
func compile(fsys fs.FS, dir string, builtin bool) (*App, error) {
	m, err := readManifest(fsys)
	if err != nil {
		return nil, err
	}
	a := &App{Manifest: m, Builtin: builtin, Dir: dir, Package: packageName(m.Name)}
	if raw, err := fs.ReadFile(fsys, ".source.json"); err == nil && !builtin {
		var src struct {
			URL string `json:"url"`
		}
		if json.Unmarshal(raw, &src) == nil {
			a.Source = src.URL
		}
	}
	if err := a.build(fsys); err != nil {
		a.Err = "编译失败：" + err.Error()
		return a, nil
	}
	if _, err := fs.Stat(fsys, "server.js"); err == nil {
		if a.ServerCode, err = buildFor(fsys, "server.js", "miband/server"); err != nil {
			a.Err = "server.js 编译失败：" + err.Error()
		}
	}
	return a, nil
}

// build 把 app 编译成快应用安装包里的文件
func (a *App) build(fsys fs.FS) error {
	opts := ux.Options{Modules: map[string]string{"wristkit": runtimeJS}, Entry: pageEntry}
	page, err := ux.CompilePage(fsys, a.Main, opts)
	if err != nil {
		return err
	}
	features := map[string]bool{}
	for _, f := range page.Features {
		features[f] = true
	}
	assets, err := quickapp.Assets(fsys)
	if err != nil {
		return err
	}
	icon := "/common/logo.png"
	if _, err := fs.Stat(fsys, "icon.png"); err == nil {
		icon = "/icon.png"
	} else {
		assets = append(assets, rpk.File{Name: "common/logo.png", Body: defaultIcon})
	}
	// 版本号：页面、图片、名字一样就一样
	h := sha1.New()
	h.Write([]byte(a.Package + "\x00" + a.Title + "\x00" + page.JS))
	for _, f := range assets {
		h.Write([]byte("\x00" + f.Name + "\x00"))
		h.Write(f.Body)
	}
	a.Version = int(binary.BigEndian.Uint32(h.Sum(nil))%2_000_000_000) + 1
	manifest := map[string]any{
		"package": a.Package, "name": a.Title, "versionName": "1.0.0", "versionCode": a.Version,
		"minPlatformVersion": 1200, "icon": icon, "deviceTypeList": []string{"watch"},
		"config":  map[string]any{"logLevel": "log", "designWidth": 212},
		"router":  map[string]any{"entry": "pages/index", "pages": map[string]any{"pages/index": map[string]any{"component": "index"}}},
		"minAPILevel": 2,
	}
	pages := []rpk.File{{Name: "pages/index/index.js", Body: []byte(page.JS)}}
	a.Files, err = quickapp.Assemble(fsys, manifest, "", pages, features, assets, opts)
	return err
}

// buildFor：lib 是运行时提供的模块名（手环上是 miband，Mac 上是 miband/server）
func buildFor(fsys fs.FS, main, lib string) (string, error) {
	exts := []string{"", ".jsx", ".js", ".json", "/index.jsx", "/index.js"}
	fsPlugin := api.Plugin{Name: "fsys", Setup: func(b api.PluginBuild) {
		b.OnResolve(api.OnResolveOptions{Filter: `.*`}, func(args api.OnResolveArgs) (api.OnResolveResult, error) {
			if args.Path == lib || args.Path == "wristkit/server" {
				return api.OnResolveResult{Path: lib, External: true}, nil
			}
			p := args.Path
			if args.Kind != api.ResolveEntryPoint {
				if !strings.HasPrefix(p, "./") && !strings.HasPrefix(p, "../") {
					return api.OnResolveResult{}, fmt.Errorf("只能 import '%s' 或 app 目录里的文件（./xxx），不支持 %q", lib, p)
				}
				p = path.Join(path.Dir(args.Importer), p)
			}
			p = path.Clean(strings.TrimPrefix(p, "/"))
			for _, ext := range exts {
				if st, err := fs.Stat(fsys, p+ext); err == nil && !st.IsDir() {
					return api.OnResolveResult{Path: p + ext, Namespace: "fsys"}, nil
				}
			}
			return api.OnResolveResult{}, fmt.Errorf("找不到 %s", p)
		})
		b.OnLoad(api.OnLoadOptions{Filter: `.*`, Namespace: "fsys"}, func(args api.OnLoadArgs) (api.OnLoadResult, error) {
			raw, err := fs.ReadFile(fsys, args.Path)
			if err != nil {
				return api.OnLoadResult{}, err
			}
			s := string(raw)
			loader := api.LoaderJSX // .js 里也允许写 JSX
			if path.Ext(args.Path) == ".json" {
				loader = api.LoaderJSON
			}
			return api.OnLoadResult{Contents: &s, Loader: loader}, nil
		})
	}}
	res := api.Build(api.BuildOptions{
		EntryPoints: []string{main},
		Bundle:      true,
		Write:       false,
		Format:      api.FormatCommonJS,
		Platform:    api.PlatformNeutral,
		Target:      api.ES2020, // 手环的 JS 引擎实测支持到 ES2020（docs/render-spike.md）
		JSX:         api.JSXTransform,
		JSXFactory:  "__h",
		JSXFragment: "__Fragment",
		Plugins:     []api.Plugin{fsPlugin},
		LogLevel:    api.LogLevelSilent,
	})
	if len(res.Errors) > 0 {
		var msgs []string
		for _, e := range res.Errors {
			loc := ""
			if e.Location != nil {
				loc = fmt.Sprintf("%s:%d:%d: ", e.Location.File, e.Location.Line, e.Location.Column)
			}
			msgs = append(msgs, loc+e.Text)
		}
		return "", errors.New(strings.Join(msgs, "\n"))
	}
	if len(res.OutputFiles) == 0 {
		return "", errors.New("esbuild 没有输出")
	}
	return string(res.OutputFiles[0].Contents), nil
}

// BuildDir 把一个 app 目录编译打包成签好名的安装包（命令行 miband pack 用）
func BuildDir(fsys fs.FS) ([]byte, *App, error) {
	a, err := compile(fsys, "", false)
	if err != nil {
		return nil, nil, err
	}
	if a.Err != "" {
		return nil, a, errors.New(a.Err)
	}
	s, err := quickapp.DebugSigner()
	if err != nil {
		return nil, a, err
	}
	data, err := rpk.Build(a.Files, "", s)
	return data, a, err
}
