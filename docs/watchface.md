# 表盘调研

想做一个表盘：仿官方的样式，下方放两个入口，点了打开 Agent 猫（或者任意 app）。这份文档记录在真机（小米手环 9 Pro，固件 3.1.190，屏幕 336×480）上调研的过程和结果。

表盘的增删改查已经做进了本体（`server/internal/xiaomi/watchface.go`，命令行 `miband faces / face-install / face-set / face-remove`）。表盘本身的生成、探测用的脚本都是一次性的，没进仓库。

## 结论先说

**9 Pro 的表盘点不开第三方快应用。** 表盘上的「跳转」只能指向固件里写死的系统应用（心率、天气、支付宝、闹钟……），按编号指，没有能填包名的地方。证据：

1. 官方表盘的跳转记录逐字节拆开，目标只有一个 16 位的系统应用编号，名字字段只是设计师标签（见「表盘文件格式」）。
2. 小米表盘商店里 700 个官方 9 Pro 表盘的介绍，跳转目标全是系统功能，没有一个跳第三方应用。
3. 其他能打开应用的路都没有：手环的 shell 里没有启动应用的命令，Lua 表盘没有打开应用的接口，卡片页的协议手环不回（见「试过、走不通的路」）。

能做的：

- **装表盘、切换、删除、列出**：协议走通，已经做进本体。
- **自己生成表盘**：用社区的打包工具能生成 9 Pro 能用的表盘，包括 Lua 表盘。
- **跳系统应用的入口**：格式已经清楚，可以做（入口只能是系统应用）。
- **调整手环应用列表的顺序 / 隐藏**：系统命令 29/30，快应用也在列表里，可以把 Agent 猫排到第一个（还没做）。

## 装表盘的协议

同 Gadgetbridge 的 `XiaomiWatchfaceService`，命令 type 4：

| subtype | 作用 |
|---|---|
| 0 | 列表（id、名字、是否当前、能不能删） |
| 1 | 设为当前（带 `watchfaceId`） |
| 2 | 删除（带 `watchfaceId`） |
| 4 | 准备安装（带 `watchfaceInstallStart{id, size}`），手环回 `installStatus`，0 可以装，2 已经有同 id 的 |

准备好之后文件走大文件通道，数据类型 16（快应用是 64），格式和装快应用一样。传完手环会主动发一条 4/5，过一两秒再发 4/1 设为当前。

表盘 id 在文件头 0x28 起，ASCII 数字。官方表盘是 12 位（如 `120917345006`），社区工具打出来的是 9 位，都能装。

## 表盘文件格式

文件头魔数 `5A A5 34 12`，整体结构见 [Mi8WfBinTool 的说明](https://github.com/zhy8388608/Mi8WfBinTool)（9 Pro 是 `n67`，和手环 8 Pro 共用一套编码）。这里只记它没写清楚、在官方表盘里看到的部分。

每页表盘有一个 0x58 字节的头，里面是若干 `(数量, 偏移)`，指向 16 字节一条的定义表；定义里的偏移、长度指向真正的数据。**属性数据在文件末尾**（图片在前面），所以只读文件开头是看不到的。

| 头里的位置 | 类型 | 内容 |
|---|---|---|
| 0x08 | 0 | 基础元素：引用（类型 + 序号）和坐标 |
| 0x18 | 2 | 图片 |
| 0x20 | 3 | 图片组 |
| 0x30 | 5 | Lua / JS 脚本 |
| 0x38 | 6 | 数据格子选项的多语言名字（`Workouts` / `运动`） |
| 0x40 | 7 | 控件（数字、进度、图片组…） |
| 0x48 | 8 | 可编辑格子：列出可选的类型 9 记录 |
| 0x50 | 9 | 跳转、数据格子选项、元素组 |

### 跳转记录（类型 9，56 字节）

从官方表盘「快捷功能」（跳活力指标、支付宝、闹钟、微信）拆出来的：

| 字节 | 内容 |
|---|---|
| 0–3 | `e7 bb 84 31` |
| 4–39 | 设计师标签，UTF-8（如「天气」「心率」），手环不用；也可以全 0 |
| 40–41 | `01 00` |
| 42–43 | u16：`(系统应用编号 << 4) \| 3` |
| 44–47 | 0 |
| 48–51 | 点击区的图片引用（类型 2） |
| 52–55 | 点击区宽、高（u16 各一） |

看到的系统应用编号：心率 `0x105`、睡眠 `0x10c`、步数 / 活力指标 `0x113`、音乐 `0x115`、设置 `0x116`、支付宝 `0x11e`、秒表 `0x120`、天气 `0x121`、微信 `0x125`、闹钟 `0x128`。

末位不是 3 而是 7 的（如 `0x1217`），是数据格子的选项，不是跳转。手环 8 Pro 时代的写法（前 36 字节填应用名，代码 `01 00 82 23`）在 9 Pro 上没反应。

### 可编辑格子

官方表盘下方能换内容的格子是类型 8：一条记录列出这个格子可选的类型 9 记录（每个选项有图标、编号和要画的控件）。用户选了哪个记在手环上的 `config.json`：

```json
"data_item_cfg": [
  { "is_app_widget": "0", "slot_uid": "134217728", "ref_id": "150994951" },
  { "is_app_widget": "1", "slot_uid": "134217731", "ref_id": "3218" }
]
```

`slot_uid` 是 `0x08000000 + 格子序号`，`ref_id` 是 `0x09000000 + 类型 9 记录序号`。`is_app_widget` 为 1 的格子嵌的是系统应用的卡片，`ref_id` 是卡片编号；格子记录里写的是 `00 00 30 00` + 卡片编号。

## 在手环上看文件：探测表盘

手环没有读文件的协议，靠 Lua 表盘：

- Lua 表盘能 `os.execute`（跑 NuttX 的 nsh 命令）、`io.open` 读写任意路径。
- `/data/quickapp/files/<包名>/` 就是那个快应用的 `internal://files/`。

所以做法是：Lua 表盘把命令输出、文件内容（二进制转成十六进制）写到一个探测 app 的目录，探测 app 用 `@system.file` 读出来，分段 `call` 给 `server.js` 存到 Mac 上。注意：

- 一次发太多会丢片，服务用 `-v` 跑，日志里有每条消息，可以拼回来；或者让 app 隔一会儿重发。
- 快应用打开时会问一次「手机端在不在」，那一刻 Mac 没连上，之后就一直发不出去。要在服务连上之后打开（`server.js` 里 `wakeBand()`），app 已经开着的话让 Mac 推一条消息触发重发。

Lua 表盘用 [Mi8WfBinTool](https://github.com/zhy8388608/Mi8WfBinTool) 打包：`wfDef.json` 里放一个 `{"type": "type_element_lua", "jsFileName": "_lua/x/main.lua"}`，脚本放 `images/_lua/x/main.lua`，`pack <目录> <输出> n67`。它的 `unpack` 遇到没有息屏页的表盘会报错，文件本身没问题。

### 手环上的路径

| 路径 | 内容 |
|---|---|
| `/system/watchface/<id>/resource.bin` | 内置表盘本体 |
| `/data/app/watchface/builtin/<id>/config.json` | 内置表盘的配置（格子选了什么） |
| `/data/app/watchface/market/<id>/` | 自己装的表盘（`resource.bin`、`config.json`、Lua 脚本） |
| `/data/app/watchface/watchface_list.json` | 表盘列表 |
| `/data/app/launcher/launcher.db` | 应用列表，UnQLite，按包名（系统应用如 `com.xiaomi.miwear.alipay`） |
| `/data/quickapp/apps.json` | 装了的快应用 |

## 官方表盘商店

公开接口，不用登录（来自 [get-mi-watchface](https://github.com/aurysian-yan/get-mi-watchface)）：

```
https://watch-appstore.iot.mi.com/api/watchface/prize/detail?model=miwear.watch.n67cn&id=<表盘 id>
```

返回里有 `config_file`（表盘文件下载地址）、`introduction`（介绍，写了能跳哪些功能）、`recommend_list`（推荐表盘，可以顺着找更多表盘）。9 Pro 国行是 `miwear.watch.n67cn`，其他型号见那个项目的 README。

官方表盘是小米和设计师的，只用来研究格式，不要放进仓库或者分发。

## 试过、走不通的路

- **按快应用名字跳转**：EasyFace 文档说可以把跳转目标写成「JS 应用名」，那是手环 8 Pro / 红米手表 3、4 的能力。9 Pro 上按名字、按各种编号做的测试按钮点了都没反应。
- **Lua 打开应用**：Lua 全局里只有 `lvgl`、`dataman`、`topic`、`vibrator`、`animengine`、`screen`（空）、`activity`（只有 `APPID = {LUA = 56, WATCHFACE = 2}` 和 `isShown`）、`navigator`（只有 `finish`），没有打开应用的接口。
- **shell 打开应用**：nsh 的命令和内置程序里没有启动应用的（有 `miwear`，是主界面进程本身）。
- **卡片页放 app**：协议里 `WidgetPart` 有 `appId` 字段，但读卡片页的系统命令 51、53 手环不回。

## 应用列表：系统命令 29 / 30

系统命令 type 2 subtype 29 读 `displayItems`，是手环应用列表的顺序，按包名，带名字、是否隐藏：

```
0  com.xiaomi.miwear.activities  活力指标
…
33 dev.wristkit.agent_cat        Agent 猫
34 dev.wristkit.tiger_cat        橘黑猫
```

快应用也在里面。subtype 30 是写（Gadgetbridge 用它排序、隐藏），可以用来把某个 app 排到第一个。还没写过，写之前先读一份存着，方便还原。

## 参考

- [Mi8WfBinTool](https://github.com/zhy8388608/Mi8WfBinTool)：表盘打包、解包，支持 n67 和 Lua 表盘
- [interactive-watchface-kit](https://github.com/ysa688422-png/interactive-watchface-kit)：n67 Lua 表盘的模板
- [MiWatchLuaWatchfaces](https://github.com/m0tral/MiWatchLuaWatchfaces)：Lua 表盘例子
- [band10-toolkit](https://github.com/utsabfdahal/band10-toolkit)：表盘格式笔记（手环 10）
- [EasyFace wiki](https://github.com/m0tral/EasyFace/wiki/Actions)：老机型的跳转按钮写法
- [get-mi-watchface](https://github.com/aurysian-yan/get-mi-watchface)：官方表盘商店接口
- [miband-hack 协议笔记](https://github.com/ptrinh/miband-hack/blob/main/docs/protocol.md)：装表盘的协议
