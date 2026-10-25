# wristkit

## 本体和 app

这个项目分两层，改动时先分清落在哪一层：

- **本体**：Mac 端的编译器、打包、app 宿主和服务 `server/internal/`（含编进每个 app 的手环端运行时 `bandapp/wristkit.js`）、App `server/app/`（含预览 `frontend/src/uxrt.ts`）、命令行 `server/cmd/`。改本体要重新构建 App，会影响所有 app。
- **app**：`server/apps/<名字>/`（内置）和用户 app 目录里的 app，各自的 `index.ux`（跑在手环上）、`server.js`（跑在 Mac 上）、`manifest.json`、图片。只影响这一个 app。

规则：

- **改本体之前，先告诉用户要改本体的哪里、为什么 app 里做不到，等用户同意再改。** 能在 app 里做的，就在 app 里做。
- 汇报改动时，按「本体」「app」分开说清楚各改了什么。

## 给所有用户用的

这是开源项目，App 和 app 是给所有用户用的，不是只给仓库作者一个人用：

- 界面上的文字用用户能懂的说法，不露实现上的概念（`server.js`、store、渲染器协议这类）。这些概念只出现在写 app 的文档（docs/apps.md）和代码里。
- 方案按所有用户的环境设计，不按作者本机的情况：接入用户环境的（改 Claude Code 设置、读本机文件）要能开关、能还原，拿不到数据的用户自动隐藏或降级，不报错。

写 app、app 能用的接口和限制见 [docs/apps.md](docs/apps.md)，写 app 的流程见 [skills/wristkit-app](skills/wristkit-app/SKILL.md)；手环上实测出来的限制（消息大小、节点创建速度、JS 引擎能力）见 [docs/render-spike.md](docs/render-spike.md)；表盘（装表盘、表盘格式、表盘跳转、在手环上看文件）见 [docs/watchface.md](docs/watchface.md)。
