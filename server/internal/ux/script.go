package ux

import (
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
	"sync"

	"github.com/evanw/esbuild/pkg/api"
)

// 脚本：<script> 用 esbuild 打成一个函数，import 的本地文件一起打进去。
// 快应用的接口（import x from '@system.vibrator'）交给手环上的 $app_require$('@app-module/system.vibrator')，
// 用到了哪些记下来，写进 manifest 的 features。

// Options 是编译时的额外设置
type Options struct {
	// Modules 是额外能 import 的模块：名字 → 源码（如 wristkit 在手环上的运行时）
	Modules map[string]string
	// Entry 非空时，页面脚本的入口换成它；页面自己写的 <script> 可以用 import def from 'ux:script' 拿到
	Entry string
}

// compileScript 返回 $app_script$ 函数的代码和用到的系统接口（如 system.vibrator）
func compileScript(fsys fs.FS, file, src string, page bool, opts Options) (string, []string, error) {
	features := map[string]bool{}
	var mu sync.Mutex // esbuild 并行解析各个 import，记 features 要加锁
	dir := path.Dir(file)
	exts := []string{"", ".js", ".json", "/index.js"}
	resolver := api.Plugin{Name: "app", Setup: func(b api.PluginBuild) {
		b.OnResolve(api.OnResolveOptions{Filter: `.*`}, func(args api.OnResolveArgs) (api.OnResolveResult, error) {
			p := args.Path
			if _, ok := opts.Modules[p]; ok {
				return api.OnResolveResult{Path: p, Namespace: "module"}, nil
			}
			if p == "ux:script" && opts.Entry != "" {
				return api.OnResolveResult{Path: file, Namespace: "script"}, nil
			}
			if strings.HasPrefix(p, "@system.") || strings.HasPrefix(p, "@service.") {
				mu.Lock()
				features[p[1:]] = true
				mu.Unlock()
				return api.OnResolveResult{Path: p[1:], Namespace: "native"}, nil
			}
			if !strings.HasPrefix(p, "./") && !strings.HasPrefix(p, "../") && !strings.HasPrefix(p, "/") {
				return api.OnResolveResult{}, fmt.Errorf("只能 import 快应用的接口（@system.xxx）或 app 目录里的文件（./xxx），不支持 %q", p)
			}
			base := dir
			if args.Importer != "" && (args.Namespace == "app" || args.Namespace == "script") {
				base = path.Dir(args.Importer)
			}
			if strings.HasPrefix(p, "/") {
				p = strings.TrimPrefix(p, "/")
			} else {
				p = path.Join(base, p)
			}
			for _, ext := range exts {
				if st, err := fs.Stat(fsys, p+ext); err == nil && !st.IsDir() {
					return api.OnResolveResult{Path: p + ext, Namespace: "app"}, nil
				}
			}
			return api.OnResolveResult{}, fmt.Errorf("找不到 %s", p)
		})
		b.OnLoad(api.OnLoadOptions{Filter: `.*`, Namespace: "native"}, func(args api.OnLoadArgs) (api.OnLoadResult, error) {
			s := "module.exports = $app_require$(" + jsString("@app-module/"+args.Path) + ")"
			return api.OnLoadResult{Contents: &s, Loader: api.LoaderJS}, nil
		})
		b.OnLoad(api.OnLoadOptions{Filter: `.*`, Namespace: "module"}, func(args api.OnLoadArgs) (api.OnLoadResult, error) {
			s := opts.Modules[args.Path]
			return api.OnLoadResult{Contents: &s, Loader: api.LoaderJS, ResolveDir: "/"}, nil
		})
		b.OnLoad(api.OnLoadOptions{Filter: `.*`, Namespace: "script"}, func(args api.OnLoadArgs) (api.OnLoadResult, error) {
			s := src
			return api.OnLoadResult{Contents: &s, Loader: api.LoaderJS}, nil
		})
		b.OnLoad(api.OnLoadOptions{Filter: `.*`, Namespace: "app"}, func(args api.OnLoadArgs) (api.OnLoadResult, error) {
			raw, err := fs.ReadFile(fsys, args.Path)
			if err != nil {
				return api.OnLoadResult{}, err
			}
			s := string(raw)
			loader := api.LoaderJS
			if path.Ext(args.Path) == ".json" {
				loader = api.LoaderJSON
			}
			return api.OnLoadResult{Contents: &s, Loader: loader}, nil
		})
	}}
	entry := src
	if opts.Entry != "" {
		entry = opts.Entry
	}
	res := api.Build(api.BuildOptions{
		Stdin:      &api.StdinOptions{Contents: entry, Sourcefile: file, Loader: api.LoaderJS},
		Bundle:     true,
		Write:      false,
		Format:     api.FormatIIFE,
		GlobalName: "__page__",
		Platform:   api.PlatformNeutral,
		Target:     api.ES2020, // 手环的 JS 引擎实测支持到 ES2020（docs/render-spike.md）
		Plugins:    []api.Plugin{resolver},
		LogLevel:   api.LogLevelSilent,
	})
	if len(res.Errors) > 0 {
		e := res.Errors[0]
		if e.Location != nil {
			return "", nil, fmt.Errorf("%s 第 %d 行：%s", e.Location.File, e.Location.Line, e.Text)
		}
		return "", nil, fmt.Errorf("%s", e.Text)
	}
	code := string(res.OutputFiles[0].Contents)
	var b strings.Builder
	b.WriteString("function __scriptModule__(module, exports, $app_require$) {\n")
	b.WriteString(code)
	b.WriteString("exports.default = __page__.default || {};\n")
	if page {
		b.WriteString(pageAccess)
	}
	b.WriteString("}")
	var list []string
	for f := range features {
		list = append(list, f)
	}
	sort.Strings(list)
	return b.String(), list, nil
}

// 页面的 private / protected / public 合成 data，照抄 aiot-toolkit（UxLoaderUtils.contenAccess）
const pageAccess = `
var moduleOwn = exports.default;
var accessors = ['public', 'protected', 'private'];
if (moduleOwn.data && accessors.some(function (acc) { return moduleOwn[acc]; })) {
  throw new Error('页面的 data 不能和 public、protected、private 同时写，用 private');
} else if (!moduleOwn.data) {
  moduleOwn.data = {};
  moduleOwn._descriptor = {};
  accessors.forEach(function (acc) {
    if (typeof moduleOwn[acc] === 'object') {
      moduleOwn.data = Object.assign(moduleOwn.data, moduleOwn[acc]);
      for (var name in moduleOwn[acc]) moduleOwn._descriptor[name] = { access: acc };
    }
  });
}
`
