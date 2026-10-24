// Package apps 是打包进 App 的内置 app，每个子目录一个（格式见 docs/apps.md）。
package apps

import "embed"

//go:embed all:agent-cat all:tiger-cat
var FS embed.FS
