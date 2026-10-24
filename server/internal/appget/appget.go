// Package appget 从 GitHub 下载 app 到用户 app 目录。
//
// 只取 app 所在的那个目录，不下整个仓库：先用 GitHub 接口查这个目录最近的一次提交、列出那次提交里的文件
// （两次接口请求；不登录每小时只能调 60 次），文件再从 raw.githubusercontent.com 按提交下载（不占接口次数）。
// 先下到 app 目录里的隐藏临时目录，检查过了再整个换进去，下到一半失败不会留下半个 app。
// 下下来的 app 目录里多一个 .source.json，记着从哪下的、哪次提交，用来检查更新。
package appget

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	MaxFiles = 500
	MaxBytes = 10 << 20
	// SourceFile 记着 app 从哪下的，放在 app 目录里
	SourceFile = ".source.json"
)

// Source 是一个 GitHub 上的 app 目录。
type Source struct {
	Owner string `json:"owner"`
	Repo  string `json:"repo"`
	Ref   string `json:"ref"`  // 分支、标签或提交；空是默认分支
	Path  string `json:"path"` // 仓库里的目录；空是仓库根目录
}

// Meta 是 .source.json 的内容。
type Meta struct {
	Source
	URL    string    `json:"url"`    // 给人看、点开的地址
	Commit string    `json:"commit"` // 下的是哪次提交（这个目录最近的一次）
	Time   time.Time `json:"time"`
}

var namePart = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// Parse 认这几种写法：
//
//	https://github.com/owner/repo/tree/<分支>/<目录>
//	https://github.com/owner/repo（仓库根目录就是 app）
//	owner/repo/<目录>（默认分支）
//	owner/repo/<目录>@<分支>
//
// 完整链接里分支名带 / 的认不出来（分不清哪段是分支），在后面加 @<分支>。
func Parse(s string) (Source, error) {
	s = strings.TrimSpace(s)
	var src Source
	if i := strings.LastIndex(s, "@"); i > 0 {
		src.Ref = s[i+1:]
		s = s[:i]
	}
	full := false
	if u, err := url.Parse(s); err == nil && u.Host != "" {
		if !strings.EqualFold(u.Host, "github.com") && !strings.EqualFold(u.Host, "www.github.com") {
			return src, errors.New("只支持 github.com 上的地址")
		}
		s = u.Path
		full = true
	} else if strings.HasPrefix(strings.ToLower(s), "github.com/") {
		s = s[len("github.com/"):]
		full = true
	}
	var parts []string
	for _, p := range strings.Split(s, "/") {
		if p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) < 2 {
		return src, errors.New("地址要写成 owner/仓库/目录，或者 GitHub 上那个目录的链接")
	}
	src.Owner, src.Repo = parts[0], strings.TrimSuffix(parts[1], ".git")
	rest := parts[2:]
	if full && len(rest) >= 2 && (rest[0] == "tree" || rest[0] == "blob") {
		if src.Ref == "" {
			src.Ref = rest[1]
		}
		rest = rest[2:]
	}
	if len(rest) > 0 && rest[len(rest)-1] == "manifest.json" { // 贴的是 manifest.json 的链接
		rest = rest[:len(rest)-1]
	}
	for _, p := range append([]string{src.Owner, src.Repo}, rest...) {
		if !namePart.MatchString(p) || p == "." || p == ".." {
			return src, fmt.Errorf("地址里的 %q 不对", p)
		}
	}
	src.Path = strings.Join(rest, "/")
	if src.Ref == "HEAD" { // URL() 里默认分支写成 HEAD
		src.Ref = ""
	}
	return src, nil
}

// URL：在 GitHub 上看这个目录的链接
func (s Source) URL() string {
	u := "https://github.com/" + s.Owner + "/" + s.Repo
	if s.Ref != "" || s.Path != "" {
		ref := s.Ref
		if ref == "" {
			ref = "HEAD"
		}
		u += "/tree/" + ref
		if s.Path != "" {
			u += "/" + s.Path
		}
	}
	return u
}

// Same：是不是同一个来源（仓库名不分大小写）
func (s Source) Same(o Source) bool {
	return strings.EqualFold(s.Owner, o.Owner) && strings.EqualFold(s.Repo, o.Repo) && s.Ref == o.Ref && s.Path == o.Path
}

// Client 访问 GitHub。API、Raw 为空用 GitHub 的地址（测试时换成假的服务）。
type Client struct {
	HTTP *http.Client
	API  string
	Raw  string
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return NewHTTPClient()
}

func (c *Client) get(ctx context.Context, u string, api bool) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, err
	}
	if api {
		req.Header.Set("Accept", "application/vnd.github+json")
	}
	resp, err := c.http().Do(req)
	if err != nil {
		if ctx.Err() == nil {
			return nil, fmt.Errorf("连不上 GitHub：%w", err)
		}
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("连不上 GitHub：%w", err)
	}
	switch {
	case resp.StatusCode == 404:
		return nil, errors.New("GitHub 上找不到，看看地址对不对（只支持公开仓库）")
	case resp.StatusCode == 403 || resp.StatusCode == 429:
		return nil, errors.New("GitHub 限制了访问次数（不登录每小时 60 次），过一会儿再试")
	case resp.StatusCode != 200:
		return nil, fmt.Errorf("GitHub 返回 %s", resp.Status)
	}
	return body, nil
}

func (c *Client) api(p string) string {
	base := c.API
	if base == "" {
		base = "https://api.github.com"
	}
	return strings.TrimSuffix(base, "/") + p
}

// Latest：这个目录最近的一次提交
func (c *Client) Latest(ctx context.Context, src Source) (string, error) {
	q := url.Values{"per_page": {"1"}}
	if src.Ref != "" {
		q.Set("sha", src.Ref)
	}
	if src.Path != "" {
		q.Set("path", src.Path)
	}
	body, err := c.get(ctx, c.api("/repos/"+src.Owner+"/"+src.Repo+"/commits?"+q.Encode()), true)
	if err != nil {
		return "", err
	}
	var commits []struct {
		SHA string `json:"sha"`
	}
	if err := json.Unmarshal(body, &commits); err != nil {
		return "", fmt.Errorf("GitHub 返回的内容看不懂：%w", err)
	}
	if len(commits) == 0 || commits[0].SHA == "" {
		return "", errors.New("GitHub 上找不到这个目录，看看地址对不对")
	}
	return commits[0].SHA, nil
}

type file struct {
	path string // 相对 app 目录
	size int64
}

// list：这次提交里 app 目录下的所有文件
func (c *Client) list(ctx context.Context, src Source, commit string) ([]file, error) {
	body, err := c.get(ctx, c.api("/repos/"+src.Owner+"/"+src.Repo+"/git/trees/"+commit+"?recursive=1"), true)
	if err != nil {
		return nil, err
	}
	var tree struct {
		Tree []struct {
			Path string `json:"path"`
			Type string `json:"type"`
			Size int64  `json:"size"`
		} `json:"tree"`
		Truncated bool `json:"truncated"`
	}
	if err := json.Unmarshal(body, &tree); err != nil {
		return nil, fmt.Errorf("GitHub 返回的内容看不懂：%w", err)
	}
	prefix := ""
	if src.Path != "" {
		prefix = src.Path + "/"
	}
	var out []file
	var total int64
	for _, e := range tree.Tree {
		if e.Type != "blob" || !strings.HasPrefix(e.Path, prefix) {
			continue
		}
		rel := e.Path[len(prefix):]
		if !safeRel(rel) {
			continue
		}
		out = append(out, file{rel, e.Size})
		total += e.Size
		if len(out) > MaxFiles {
			return nil, fmt.Errorf("文件太多（超过 %d 个），看看地址是不是指到了 app 的目录", MaxFiles)
		}
		if total > MaxBytes {
			return nil, fmt.Errorf("太大了（超过 %dMB），看看地址是不是指到了 app 的目录", MaxBytes>>20)
		}
	}
	if len(out) == 0 && tree.Truncated {
		return nil, errors.New("仓库太大，GitHub 没列全，下载不了")
	}
	return out, nil
}

// safeRel：路径不能跳出 app 目录，也不要隐藏文件（.git、.source.json 这些）
func safeRel(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." || strings.HasPrefix(seg, ".") {
			return false
		}
	}
	return path.Clean(p) == p
}

func (c *Client) raw(src Source, commit, rel string) string {
	base := c.Raw
	if base == "" {
		base = "https://raw.githubusercontent.com"
	}
	p := rel
	if src.Path != "" {
		p = src.Path + "/" + rel
	}
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.TrimSuffix(base, "/") + "/" + src.Owner + "/" + src.Repo + "/" + commit + "/" + strings.Join(segs, "/")
}

var appName = regexp.MustCompile(`^[a-z0-9-]+$`)

// Result 是下载的结果。
type Result struct {
	Name string // app 名字（manifest.json 的 name），也是 app 目录下的文件夹名
	Dir  string
	Meta Meta
	Prev *Meta // 原来就有这个 app（更新）时，原来的来源
}

// Install 把 app 下到 appDir/<name>/。同名的 app 已经在了：同一个来源的就更新，不是的报错，不覆盖。
func (c *Client) Install(ctx context.Context, input, appDir string) (*Result, error) {
	src, err := Parse(input)
	if err != nil {
		return nil, err
	}
	commit, err := c.Latest(ctx, src)
	if err != nil {
		return nil, err
	}
	files, err := c.list(ctx, src, commit)
	if err != nil {
		return nil, err
	}
	hasManifest := false
	for _, f := range files {
		hasManifest = hasManifest || f.path == "manifest.json"
	}
	if !hasManifest {
		return nil, errors.New("这个目录里没有 manifest.json，不是 app 的目录")
	}
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp(appDir, ".download-") // 隐藏目录，app 宿主不会当成 app
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	if err := os.Chmod(tmp, 0o755); err != nil { // MkdirTemp 建的是 0700
		return nil, err
	}
	for _, f := range files {
		data, err := c.get(ctx, c.raw(src, commit, f.path), false)
		if err != nil {
			return nil, fmt.Errorf("下载 %s：%w", f.path, err)
		}
		if len(data) > MaxBytes {
			return nil, fmt.Errorf("%s 太大了", f.path)
		}
		target := filepath.Join(tmp, filepath.FromSlash(f.path))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(target, data, 0o644); err != nil {
			return nil, err
		}
	}
	var m struct {
		Name string `json:"name"`
	}
	raw, _ := os.ReadFile(filepath.Join(tmp, "manifest.json"))
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("manifest.json 不对：%w", err)
	}
	if !appName.MatchString(m.Name) {
		return nil, fmt.Errorf("manifest.json 里的 name %q 不对：只能用小写字母、数字和 -", m.Name)
	}
	meta := Meta{Source: src, URL: src.URL(), Commit: commit, Time: time.Now()}
	metaRaw, _ := json.MarshalIndent(meta, "", "  ")
	if err := os.WriteFile(filepath.Join(tmp, SourceFile), metaRaw, 0o644); err != nil {
		return nil, err
	}

	dst := filepath.Join(appDir, m.Name)
	res := &Result{Name: m.Name, Dir: dst, Meta: meta}
	if _, err := os.Stat(dst); err == nil {
		prev, err := ReadMeta(dst)
		if err != nil || !prev.Source.Same(src) {
			return nil, fmt.Errorf("app 目录里已经有 %s 了，不是从这个地址下的。先删掉它或者改个名字再下", m.Name)
		}
		res.Prev = prev
		old := tmp + "-old"
		if err := os.Rename(dst, old); err != nil {
			return nil, err
		}
		if err := os.Rename(tmp, dst); err != nil {
			_ = os.Rename(old, dst)
			return nil, err
		}
		_ = os.RemoveAll(old)
		return res, nil
	}
	if err := os.Rename(tmp, dst); err != nil {
		return nil, err
	}
	return res, nil
}

// ReadMeta 读 app 目录里的 .source.json；不是从 GitHub 下的返回错误。
func ReadMeta(dir string) (*Meta, error) {
	raw, err := os.ReadFile(filepath.Join(dir, SourceFile))
	if err != nil {
		return nil, err
	}
	var m Meta
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	if m.Owner == "" || m.Repo == "" {
		return nil, errors.New(SourceFile + " 不对")
	}
	return &m, nil
}

// HasUpdate：这个目录在 GitHub 上有没有比下的时候新的提交
func (c *Client) HasUpdate(ctx context.Context, m *Meta) (bool, error) {
	commit, err := c.Latest(ctx, m.Source)
	if err != nil {
		return false, err
	}
	return commit != m.Commit, nil
}
