package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"miband/internal/appget"
)

// 从 GitHub 下载 app、检查更新、删除用户 app。下好、删掉之后 app 宿主自己会发现（每 2 秒看一次 app 目录）。

func (s *Service) github() *appget.Client {
	return &appget.Client{HTTP: appget.NewHTTPClient()}
}

// DownloadApp 从 GitHub 下载 app 到用户 app 目录；同一个来源的已经在了就更新。返回 app 名字。
// 新下的 app 清掉这个名字以前的授权：授权按名字记，不清的话一个同名、要的权限一样的 app 会直接继承别人的授权。
// 更新（同一个来源）保留授权；新版要的权限变了，照样要重新允许。
func (s *Service) DownloadApp(ctx context.Context, input string) (string, Config, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	res, err := s.github().Install(ctx, input, AppDir())
	if err != nil {
		s.mu.Lock()
		cfg := s.cfg
		s.mu.Unlock()
		return "", cfg, err
	}
	if res.Prev == nil {
		return res.Name, s.dropGrant(res.Name), nil
	}
	s.mu.Lock()
	cfg := s.cfg
	s.mu.Unlock()
	return res.Name, cfg, nil
}

// userApp：用户 app 目录里的这个 app 的目录
func (s *Service) userApp(name string) (string, error) {
	root := AppDir()
	for _, a := range s.host.Apps() {
		if a.Name == name && !a.Builtin && a.Dir != "" {
			if rel, err := filepath.Rel(root, a.Dir); err != nil || rel == "." || strings.HasPrefix(rel, "..") || strings.Contains(rel, string(filepath.Separator)) {
				return "", fmt.Errorf("%s 不在 app 目录里", name)
			}
			return a.Dir, nil
		}
	}
	return "", fmt.Errorf("app 目录里没有 %s", name)
}

// CheckAppUpdate：从 GitHub 下的 app 有没有新版本
func (s *Service) CheckAppUpdate(ctx context.Context, name string) (bool, error) {
	dir, err := s.userApp(name)
	if err != nil {
		return false, err
	}
	m, err := appget.ReadMeta(dir)
	if err != nil {
		return false, errors.New("这个 app 不是从 GitHub 下的")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return s.github().HasUpdate(ctx, m)
}

// UpdateApp：按 .source.json 记的来源重新下一遍
func (s *Service) UpdateApp(ctx context.Context, name string) (Config, error) {
	dir, err := s.userApp(name)
	if err != nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.cfg, err
	}
	m, err := appget.ReadMeta(dir)
	if err != nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.cfg, errors.New("这个 app 不是从 GitHub 下的")
	}
	_, cfg, err := s.DownloadApp(ctx, m.URL)
	return cfg, err
}

// DeleteApp 把用户 app 的目录移到废纸篓（能找回来），收回它的权限。手环上装的不管，要卸载另外点。
func (s *Service) DeleteApp(name string) (Config, error) {
	dir, err := s.userApp(name)
	if err != nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.cfg, err
	}
	if err := moveToTrash(dir); err != nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.cfg, fmt.Errorf("移到废纸篓失败：%w", err)
	}
	return s.dropGrant(name), nil
}

func (s *Service) dropGrant(name string) Config {
	s.mu.Lock()
	_, had := s.cfg.PluginGrants[name]
	delete(s.cfg.PluginGrants, name)
	cfg := s.cfg
	grants := copyGrants(s.cfg.PluginGrants)
	s.mu.Unlock()
	if had {
		s.host.SetGrants(grants)
	}
	s.changed()
	return cfg
}

// moveToTrash 移到 ~/.Trash，重名的加上时间
func moveToTrash(dir string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	trash := filepath.Join(home, ".Trash")
	dst := filepath.Join(trash, filepath.Base(dir))
	if _, err := os.Lstat(dst); err == nil {
		dst += " " + time.Now().Format("15.04.05")
	}
	return os.Rename(dir, dst)
}
