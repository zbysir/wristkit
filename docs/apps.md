# 写 app

手环上的每个 app 是一个单独的快应用（小米手环跑的 Xiaomi Vela 系统的应用格式），手环的应用列表里能看到。Agent 猫是内置的 app，你也可以自己写。

app 用快应用的原生写法：一个 `.ux` 文件，里面是模板、脚本、样式三段，写法像 Vue。App 把它编译、打包、签名，装到手环上（一个约 10 秒），不用装 Node 和小米的开发工具。界面在手环上原生运行，点击、翻页都很快。要读 Mac 上的数据（文件、命令），再写一个在 Mac 上跑的 `server.js`，数据推给手环上的 app。

内置 app 的源码在 `server/apps/` 下，可以当例子看。

## 放在哪

```
~/Library/Application Support/miband-pet/apps/
  my-app/
    manifest.json
    index.ux
    server.js      # 可选，在 Mac 上跑
    icon.png       # 可选，手环上的图标
    assets/        # 可选，图片
```

App 里「App」卡片上有「打开 app 目录」。改了文件，App 左边的预览 2 秒内就更新；装到手环上要在 App 里点「装到手环」（改过之后显示「更新」）。和内置 app 同名（`name` 相同）的用户 app 会替换内置的那个。

`manifest.json`：

```json
{
  "name": "my-app",
  "title": "我的 app",
  "order": 100
}
```

- `name`：唯一名字，只用小写字母、数字和 `-`。手环上的包名是 `dev.wristkit.<name>`（`-` 换成 `_`）。
- `title`：手环上显示的名字。
- `order`：在 App 里的顺序，小的在前。Agent 猫是 0，不写默认 100。
- `permissions`：`server.js` 要用的能力，见下面「电脑上跑的部分：server.js」。
- `settings`：app 的配置项，见下面「配置」。
- `main`：页面文件，默认 `index.ux`。

## 最小的 app

```html
<!-- index.ux -->
<template>
  <div class="page" @click="add">
    <text class="title">计数</text>
    <text class="n">{{n}}</text>
  </div>
</template>

<script>
export default {
  private: { n: 0 },
  add() {
    this.n++
  }
}
</script>

<style>
.page { width: 212px; flex-direction: column; justify-content: center; align-items: center; background-color: #000000; }
.title { font-size: 18px; font-weight: bold; color: #ffffff; }
.n { font-size: 40px; color: #2ea0ff; }
</style>
```

## .ux 的写法

和快应用官方的写法一样（[快应用文档](https://iot.mi.com/vela/quickapp/zh/guide/)），常用的：

- **模板**：只能有一个根元素。`div` 是容器（只有 flex 布局，**默认横排**），`text` 放文字，`image` 放图片（`src="/assets/a.png"`，路径从 app 目录算），`swiper` 是跟手翻页。数据用 `{{表达式}}`；`if` / `elif` / `else` 条件显示；`for="{{list}}"` 循环（`$item` 是这一项，`$idx` 是下标；也可以写 `for="(i, item) in list"`），列表项加 `tid="id"` 按 id 复用；`@click="方法"`、`@click="方法(参数)"`、`@longpress`、`@swipe` 是事件；`<block>` 只用来包 `if` / `for`，不产生元素。
- **脚本**：`export default { private: { 数据 }, onInit() {}, onShow() {}, onHide() {}, 方法() {} }`。改 `this.数据` 界面自动更新，只更新用到它的地方。可以 import app 目录里的 js（`./util.js`）和快应用的接口（`import vibrator from '@system.vibrator'`、`@system.storage`、`@system.device`…，用到的会自动写进 manifest）。
- **样式**：写在 `<style>` 里，选择器只能是单个 `.class`、`#id` 或标签名（不支持后代选择器）。单位是 px，屏幕按 212 宽设计，高度用 `@system.device` 的 `getInfo` 算（手环 10 Pro 约 303）。动态的样式写在 `style="color: {{c}}"` 里。

**排版写在 class 里，只把会变的颜色、宽高写进 `style="…{{}}…"`**：这是原生快应用快的原因，也最不容易出排版问题。

## 和 Mac 通信：wristkit

页面里可以 `import { call, store, config } from 'wristkit'`。App 编译时自动给页面接上和 Mac 的连接，页面不用自己连：

| 名字 | 说明 |
|---|---|
| 页面方法 `onStore(name, value)` | Mac 推来数据时调用：本 app 的 `server.js` `publish(name, value)` 的，和配置（`name` 是 `config`）。打开 app 时 Mac 会把全部数据推一遍 |
| 页面方法 `onConfig(config)` | 配置到了时调用（见「配置」） |
| `call(name, ...args)` | 让本 app 的 `server.js` 做事（交给它 `on(name, fn)` 注册的函数）。不等结果：`server.js` 做完 `publish` 新数据，`onStore` 收到 |
| `store(name)` / `config()` | 现在的某项数据 / 配置 |
| `console.log` | 输出发回 Mac，在 App 的日志里；页面方法里抛的异常也会发回去，显示在 App 里 |

数据会在手环上存一份，下次打开 app 先用它画，不用等 Mac。

## 配置

app 在 `manifest.json` 的 `settings` 里声明配置项，App 照着画出设置表单；用户改了，值推给手环上的 app（页面的 `onConfig(config)`，也会以名字 `config` 调 `onStore`）和 server.js（`getConfig()`），存在 App 的配置文件里。

```json
"settings": [
  {
    "key": "quota",
    "title": "首页显示哪个额度",
    "description": "猫的标题下面那根进度条",
    "type": "select",
    "options": [{ "value": "auto", "label": "用得最多的" }, { "value": "codex", "label": "Codex" }],
    "default": "auto"
  }
]
```

`type` 可以是 `select`（要有 `options`）、`bool`、`number`、`text`。`onConfig` 拿到的是 `{ quota: 'auto' }` 这样的对象，没改过的项是 `default`。

想在手环上直接切换的东西（比如点一下换个显示方式），放在页面的数据里、或者用 `@system.storage` 存就行，不用声明成配置。

## 电脑上跑的部分：server.js

app 的界面在手环上跑，读不到 Mac 上的文件、也不能执行命令。要这些就在 app 目录里再放一个 `server.js`，它在 Mac 上跑（App 里），和手环上的界面配合。手环上的界面像网页前端，`server.js` 像它的后端；它不是网络服务，不开端口：

```js
// server.js
import { on, publish, notifyMac } from 'wristkit/server'

on('hello', (who) => {
  notifyMac('手环上点了一下', `来自 ${who}`) // Mac 右上角弹通知
  publish('lastClick', Date.now()) // 手环上页面的 onStore('lastClick', 值) 收到
})
```

```html
<!-- index.ux -->
<template>
  <div class="page">
    <text class="btn" @click="hello">点我通知 Mac</text>
    <text class="hint">{{last}}</text>
  </div>
</template>

<script>
import { call } from 'wristkit'

export default {
  private: { last: '还没点过' },
  onStore(name, value) {
    if (name === 'lastClick') this.last = new Date(value).toLocaleTimeString()
  },
  hello() {
    call('hello', '手环')
  }
}
</script>

<style>
.page { width: 212px; flex-direction: column; padding: 16px; }
.btn { font-size: 16px; color: #ffffff; }
.hint { font-size: 11px; color: #8a8f98; }
</style>
```

```json
// manifest.json
{ "name": "hello", "title": "你好", "permissions": { "notify": true } }
```

`wristkit/server` 里有（以前叫 `miband/server`，也还能用）：

| 名字 | 说明 |
|---|---|
| `on(name, fn)` | 手环上 `call(name, ...args)` 时调用 `fn(...args)` |
| `publish(name, value)` | 推数据给手环，本 app 页面的 `onStore(name, value)` 收到（app 没开着时先存着，打开时推） |
| `getStore(name)` / `onStore(name, fn)` | 读本 app `publish` 过的数据和 `band`（`{connected, foreground}`：手环连着没有、手环上有没有 app 开着）/ 数据变了时调用 |
| `getConfig()` / `onConfig(fn)` | app 的配置 / 用户改了配置时调用 |
| `storage.get(key)` / `storage.set(key, value)` | Mac 这边的持久化数据（`~/Library/Application Support/miband-pet/plugin-data/<name>.json`） |
| `notifyMac(title, body)` / `notifyBand(title, body)` | Mac 弹通知 / 手环发通知（震动）。要 `notify` 权限 |
| `wakeBand()` | 把手环点亮并打开本 app（不震动）。要 `notify` 权限；手环没连上、或 app 还没装到手环上时抛异常 |
| `readFile(path, { tail })` / `readDir(path)` / `stat(path)` | 读文件（`tail` 只读最后多少字节，适合大日志）。只能读 `read` 权限里列的目录 |
| `exec(cmd, args)` | 执行命令，返回 `{ code, stdout, stderr }`，15 秒超时。只能执行 `exec` 权限里列的命令；找不到命令时抛异常（从访达打开的 App 的 PATH 很短，所以还会找 `~/.local/bin`、`/opt/homebrew/bin`、`/usr/local/bin` 这些地方） |
| `home` | 用户目录 |
| `setTimeout` `setInterval` `console.log` | 照常用，`console` 输出在 App 的日志里 |

App 有时会让 app 马上刷新数据（比如打开「显示 Claude Code 额度」之后），会调用所有 app 的 server.js 里 `on('refresh', fn)` 注册的函数；读外部数据的 app 注册一下。

**权限**：`manifest.json` 的 `permissions` 里声明 `notify`（发通知、点亮手环）、`read`（能读的目录，可以用 `~`）、`exec`（能执行的命令名，如 `["herdr"]`）。用户 app 要了权限的，要在 App 里点「允许」，`server.js` 才会跑；权限改了要重新允许。没要权限的 `server.js` 直接跑。内置 app 不用问。

例子：Agent 猫的 `server/apps/agent-cat/herdr.js` 每 3 秒跑一次 `herdr agent list`，推 agent 状态、记每天完成了几个、跑完和卡住时提醒；`server.js` 读 Claude Code 和 Codex 的额度，`publish('usage', …)` 给手环。

## 在 Mac 上预览和调试

App 左边的预览跑的是装到手环上的同一份编译好的代码，数据也是 Mac 推给手环的同一份：可以点，按住拖动翻页，右键当长按；`call()` 会真的交给 `server.js`。改了 app 文件，预览马上更新，不用每次都装到手环。

预览是浏览器画的，字体、抗锯齿和手环上会有点差别，最后还是在手环上看一眼。

## 限制

实测数据见 [render-spike.md](render-spike.md)：

- **server.js 跑在 Mac 上，界面跑在手环上**，两边只能通过 `call` 和 `publish` 传数据（经蓝牙，几十毫秒）。很大的数据会拆成几片发。
- **新建元素比改已有元素慢**（10 Pro 上约 10ms 一个，9 Pro 上约 30ms）：一页控制在几十个元素以内，长列表分页，`if` 切换的东西别太大。
- **逐帧动画用 `image-animator`**：`images` 是帧列表（`[{ src }]`），**`duration` 是每帧停多久**（不是一轮的时长），`iteration="infinite"` 一直循环。动画由手环系统播，不占 JS。换了帧列表要等它换上（约 300ms）再调 `this.$element(id).start()`，太早调不生效（见 Agent 猫的 `startAnim`）。每帧停两三百毫秒以上比较省电。
- **手环的 JS 比 Mac 慢上百倍**（10 万次循环约 0.3 秒）。重计算放到 `server.js` 里做。
- 支持到 ES2020 的语法；只能 import 快应用的接口、`wristkit` 和 app 目录里的文件。

## 出错了怎么办

编译失败（模板写错、表达式写错、import 了不支持的东西）时，App 里这个 app 下面会显示错误，预览也会显示，改好保存后自动重新编译。手环上运行时出的错（页面方法里抛的异常）会发回 Mac，也显示在 App 里。

## 分享 app：放到 GitHub 上

把 app 的目录（有 `manifest.json` 的那个）放进一个公开的 GitHub 仓库，别人在 App 的「App」卡片底部粘贴地址就能下载。这几种写法都行：

- 目录的链接：`https://github.com/<owner>/<仓库>/tree/main/apps/my-app`
- 简写：`<owner>/<仓库>/apps/my-app`（默认分支），指定分支：`<owner>/<仓库>/apps/my-app@dev`
- 仓库根目录就是 app：`https://github.com/<owner>/<仓库>`

下载下来的 app 放在 app 目录里，和自己放进去的一样（文件夹名是 `manifest.json` 的 `name`），多一个 `.source.json` 记着从哪下的、是哪次提交。App 里能「检查新版本」「下载新版本」，下好了再点「更新」装到手环。

- 只取这个目录，不下整个仓库；隐藏文件（`.` 开头的）不下。最多 500 个文件、10MB。
- app 目录里已经有同名的、不是从这个地址下的 app，不会覆盖，要先删掉或者改名。
- 新下的 app 要了权限的，要在 App 里点「允许」，以前同名 app 的授权不算；从同一个地址更新不用重新允许（要的权限变了还是要）。
- 用的是 GitHub 不登录的接口，每小时最多 60 次（下载、检查新版本各用 1～2 次）。会走 `HTTPS_PROXY` 这些环境变量，没有的话用 macOS 系统设置里的代理（不认自动代理配置 PAC）。

## 原理

- 编译（`server/internal/ux`）：`.ux` 编成手环上快应用框架能直接加载的 JS（模板变成 `aiot.__ce__` 这些调用，样式变成对象，脚本用 esbuild 打包），格式和小米的 aiot-toolkit 编出来的一样（测试里和它的结果比对，见 `tools/ux-oracle.sh`）。
- 打包（`server/internal/quickapp`、`server/internal/rpk`）：manifest、编好的 JS、图片打成 rpk，用调试证书签名。
- 宿主（`server/internal/bandapp`）：管理所有 app，跑 `server.js`，按包名给手环上开着的 app 推数据；`wristkit.js` 是编进每个 app 的手环端运行时（连 Mac、收数据、缓存）。
- 预览（`server/app/frontend/src/uxrt.ts`）：在浏览器里模拟快应用框架，跑同一份编译好的 JS。
