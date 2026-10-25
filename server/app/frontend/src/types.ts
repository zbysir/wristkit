// 和 Go 侧 AppState（app/app.go）、service.State 对应
export interface Settings {
    address: string
    pet: 'orange' | 'gray' | 'white' // 菜单栏的猫
    claudeUsage: boolean // 接管了 Claude Code 的状态栏来拿额度
}

// 和 Go 侧 usage.Source 对应
export interface UsageSource {
    agent: 'claude' | 'codex' | string
    windows: { name: string, used: number, resetsAt: number }[]
    updated: number
}

export interface AppState {
    configured: boolean
    configPath: string
    setupError: string
    installing: string
    installProgress: number
    connected: boolean
    transport: '' | 'classic' | 'ble'
    lastError: string
    packages: string[] | null // 手环上装了的 app（包名）
    versions: Record<string, number> | null // 手环上装了的快应用 → 版本号（versionCode）
    foreground: string
    usage: UsageSource[] | null
    apps: AppInfo[] | null
    appDir: string
    legacy: string[] | null // 手环上还装着的旧版快应用
    lastPush: number
    battery: { level: number, charging: boolean } | null // 手环电量；没读到是 null
    settings: Settings | null
}

// 和 Go 侧 bandapp.Info 对应
export interface AppInfo {
    name: string
    title: string
    builtin: boolean
    dir: string
    source: string // 从 GitHub 下的：来源链接
    error: string
    package: string // 手环上的包名
    version: number // 安装包的版本号，和手环上装的不一样就要更新
    foreground: boolean // 手环上正开着
    hasServer: boolean // 带 server.js
    permissions: string[] | null // server.js 要的权限
    needGrant: boolean // 要了权限还没允许，server.js 没跑
    permKey: string
    settings: AppSetting[] | null // manifest.json 里声明的配置项
    config: Record<string, unknown> | null // 现在的配置
}

// 和 Go 侧 bandapp.Setting 对应
export interface AppSetting {
    key: string
    title: string
    description?: string
    type: 'select' | 'bool' | 'number' | 'text'
    options?: { value: string, label: string }[]
    default: unknown
}
