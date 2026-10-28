# App 里的预览

App 窗口左边的预览，在浏览器里模拟手环上的快应用框架，跑装到手环上的同一份编译好的代码。这份文档讲它怎么做的、为什么这么做、和真机还差在哪。

代码：`server/app/frontend/src/uxrt.ts`（模拟快应用框架）、`server/app/frontend/src/Preview.tsx`（把 app 跑在窗口里、接到 Mac）、`server/app/app.go` 里的 `AppPreview`、`AppStores`、`AppCall`。

## 为什么这么做

目标是改了 app 不用每次都装到手环（装一次约 10 秒，还会断开手环上正开着的这个 app），而且预览里看到的要和手环上一致。

所以预览不另外解释 `.ux`，也不另写一份界面：Go 把 `.ux` 编成手环能直接加载的 JS（见 [apps.md](apps.md)「原理」），预览加载的就是这份 JS，只把手环上的快应用框架换成浏览器里的模拟。这样模板、表达式、脚本、`wristkit.js` 的行为都和手环上是同一份代码，编译器的问题在预览里也能看出来；要模拟的只有框架那一层，范围小、好对照。

## 整体流程

```
.ux ──Go 编译──▶ app.js + pages/index/index.js + 图片 ──┬─▶ 打包成 rpk 装到手环
                                                        └─▶ AppPreview 给预览 ─▶ uxrt.ts 跑起来
```

1. `Preview.tsx` 调 `AppPreview(name)` 拿编好的 `app.js`、页面 JS 和图片（PNG 转成 data URL），交给 `uxrt.run()`。
2. `uxrt.run()` 加载两份 JS，建页面、调 `onInit` / `onReady`；窗口看得见就调 `onShow`，并当成连上了 Mac（触发 interconnect 的 `onopen`）。
3. 页面里的 `wristkit.js` 和在手环上一样发 `hello`，预览收到后从 `AppStores()` 拉这个 app 的全部数据推进去；之后 Mac 上数据一变（App 收到通知后合并 100ms 发一次 `stores` 事件），只推变了的那几项。
4. 页面 `call()` 发出的消息，预览交给 `AppCall`，和手环上发来的一样交给 `server.js` 处理。
5. app 文件改了，Mac 每 2 秒检查一次，重新编译后版本号变了，预览把旧的销毁、用新代码重新跑。

## 模拟了什么

### 加载代码

编好的模块是 `export default function (global, globalThis, window, $app_exports$, $app_evaluate$) {…}`。预览用 `new Function` 包一层，传进去模拟的 `aiot`、`$app_require$`、`console`，`global` 换成只带 `setTimeout` / `setInterval` 的对象（记下所有定时器，销毁时一起清掉）。

先跑 `app.js`，再跑页面；页面在 `$app_exports$.entry` 里登记自己，拿到页面定义（`data`、`template`、`style`、方法）。

### 数据和更新

页面的 `data` 用 `@vue/reactivity` 变成响应式，方法挂到同一个对象上，再加上 `$app`、`$element(id)`。

编好的模板是一个函数，调 `aiot.__ce__`（元素）、`__ci__`（`if`）、`__cf__`（`for`）生成界面描述，用到数据的属性都是函数。预览给每个这样的属性各建一个 effect：数据变了只更新用到它的那几处，和手环上的更新粒度一样，`if` 切换、`for` 列表变了才会新建或删掉元素。

- `if`：条件变了才把这一支整个拆掉或挂上。
- `for`：列表一变整个重建（手环上也是新建节点慢，长列表要分页，见 [render-spike.md](render-spike.md)）。
- 每一支在自己的 effect 作用域里挂，拆的时候连同里面的 effect 一起停掉；首尾各放一个注释节点做标记，只删两个标记之间的节点。

### 元素和样式

| 手环上 | 预览里 |
|---|---|
| `div`（和其他容器） | `div`，只有 flex 布局、默认横排、边框写了宽度和颜色就是实线 |
| `text`、`span`、`a` | 文字，默认白色 14px，不收缩（一行放不下时不会被压窄折行，和手环一样）；`lines` 用省略截断模拟 |
| `image` | `img`，按像素放大不模糊 |
| `image-animator` | `img` 按 `duration`（每帧停多久）换帧；`$element(id)` 有 `start` / `stop` / `pause` / `resume` |
| `swiper` | 按住拖动跟着走，松手拖过 20% 就翻页，否则弹回；`index` 变了翻过去；`$element(id).swipeTo({index})` |

样式按「标签 < class < id < 写在元素上的」合并，数字值加 `px`。`show` 为假时隐藏。

预览屏幕按快应用的设计宽度画（212×303），用 CSS `zoom` 放大 1.2 倍；拖动距离按缩放换算回去。

### 事件

- 点击 → `click`，里外两层都能点时只算最里面的；拖动过的那一下不算点击。
- 右键 → `longpress`。
- 按住划过 30px 以上 → `swipe`（`up` / `down` / `left` / `right`）。

### 快应用接口

| 接口 | 预览里 |
|---|---|
| `@system.interconnect` | 接到 Mac：发出的消息交给 `Preview.tsx` 处理（`hello`、`bye`、`call`、`log`），Mac 的数据从 `onmessage` 推进来 |
| `@system.storage` | 存在浏览器的 `localStorage` 里，按 app 分开（前缀 `preview:<名字>:`），和手环上存的不通 |
| `@system.device` | `getInfo` 返回预览屏幕的宽高 |
| `@system.vibrator` | 屏幕闪一下 |

其他接口返回空对象。`$app.exit()` 什么也不做。

### 生命周期

- 窗口藏到菜单栏、最小化、被整个挡住，或者预览滚出视野，调 `onHide`（`wristkit.js` 发 `bye`，预览停止推数据）；看得见了再 `onShow`（重新 `hello`，重新拉全部数据）。这和手环上 app 退到后台、再回来一样。
- 切换 app、app 重新编译或关掉窗口时，调 `onHide`、`onDestroy`，清掉定时器和 effect。
- 页面方法里抛的异常、`console.error` 显示在预览下面。

## 和真机的差别

预览是用来快速看界面和交互的，最后还是要在手环上看一眼。已知的差别：

- **画法**：字体、抗锯齿、默认样式是浏览器的，和手环会有点差别；样式只模拟了上面说的那些，手环不支持的 CSS 在预览里可能照样生效。
- **速度**：浏览器的 JS 比手环快上百倍，新建元素也快得多。预览里不卡，手环上可能卡；元素多的页面、重计算要按 [render-spike.md](render-spike.md) 的数字估。
- **数据**：Mac 推给预览不经蓝牙，没有延迟，也不分片；数据很大时手环上会慢。
- **存储**：预览的 `storage` 和手环上是两份，`wristkit.js` 缓存在手环上的数据预览里没有，反过来也一样。
- **只有一个页面**：只加载 `pages/index`，没有页面跳转。
- **没模拟的**：元素尺寸（`getBoundingClientRect`）、手环的系统手势和按键、其他快应用接口。

## 另一个模拟：Go 测试里的手环

`server/internal/bandapp/host_test.go` 在 goja 里也模拟了一个手环（`fakeBand`），跑编好的安装包，测 app 和 Mac 之间的数据收发、`call()`、显示逻辑。它和预览各管一头：测试不画界面，用来在 CI 里自动检查；预览画界面，给人看和点。两边模拟的 `aiot` 是各写各的，改了编译器的输出格式两边都要跟着改。
