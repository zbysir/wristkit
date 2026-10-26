import {useCallback, useEffect, useState} from 'react'
import type {ReactNode} from 'react'
import {Bell, Download, Zap, FolderOpen, Gauge, LayoutGrid, PawPrint, RefreshCw, Watch} from 'lucide-react'
import {
    AllowApp, CheckAppUpdate, CopyBuiltin, DeleteApp, DownloadApp, GetState, InstallApp, Launch, OpenApp, OpenAppDir, RevokeApp,
    SetAppConfig, SetClaudeUsage, SetPet, TestNotify, Uninstall, UpdateApp,
} from '../wailsjs/go/main/App'
import {BrowserOpenURL, EventsOn} from '../wailsjs/runtime/runtime'
import {Badge} from '@/components/ui/badge'
import {Button} from '@/components/ui/button'
import {Card, CardContent, CardDescription, CardHeader, CardTitle} from '@/components/ui/card'
import {Input} from '@/components/ui/input'
import {Select, SelectContent, SelectItem, SelectTrigger, SelectValue} from '@/components/ui/select'
import {Switch} from '@/components/ui/switch'
import {ToggleGroup, ToggleGroupItem} from '@/components/ui/toggle-group'
import {Wizard} from './Wizard'
import {Preview} from './Preview'
import {SKINS} from './Pet'
import type {Skin} from './Pet'
import type {AppInfo, AppSetting, AppState} from './types'
import {bandStatus} from './status'

type Run = (label: string, fn: () => Promise<unknown>) => void

export default function App() {
    const [state, setState] = useState<AppState | null>(null)
    const [toast, setToast] = useState('')
    const [wizard, setWizard] = useState(false)
    const [selected, setSelected] = useState('')

    const refresh = useCallback(() => {
        GetState().then(raw => {
            const s = raw as unknown as AppState
            setState(prev => {
                if (!prev && !s.configured) setWizard(true) // 第一次打开、还没配置：走连接向导
                return s
            })
        })
    }, [])

    useEffect(() => {
        refresh()
        const off = EventsOn('state', refresh)
        const t = setInterval(refresh, 30_000)
        return () => {
            off()
            clearInterval(t)
        }
    }, [refresh])

    const run: Run = useCallback((label, fn) => {
        fn().then(() => setToast(label + '：完成'))
            .catch(e => setToast(label + '失败：' + String(e)))
            .finally(() => setTimeout(() => setToast(''), 3000))
    }, [])

    if (!state) return null
    if (wizard || !state.configured) {
        return <Wizard state={state} onFinish={() => setWizard(false)}/>
    }

    // 左边预览的是哪个 app：手动选过的 → 手环上正开着的 → 第一个
    const apps = state.apps ?? []
    const app = apps.find(p => p.name === selected) ?? apps.find(p => p.foreground) ?? apps[0]

    return (
        <div className="grid h-screen grid-cols-[minmax(320px,380px)_1fr]">
            <div className="drag fixed inset-x-0 top-0 z-10 h-11 bg-background"/>
            {/* 两栏各自滚动；内容包一层，不然高度固定的 flex 列会把卡片压扁 */}
            <aside className="overflow-y-auto border-r px-6 pt-14 pb-6">
                <div className="flex flex-col gap-4">
                    <div className="flex items-center justify-between">
                        <h1 className="text-base font-semibold">预览</h1>
                        <StatusBadge state={state}/>
                    </div>
                    <Preview state={state} run={run} app={app} onSelect={setSelected}/>
                </div>
            </aside>
            <main className="overflow-y-auto px-6 pt-14 pb-6">
                <div className="flex flex-col gap-4">
                {app && (app.settings?.length ?? 0) > 0 && <AppSettingsCard app={app} run={run}/>}
                <AppsCard state={state} run={run} onSelect={setSelected}/>
                <MenubarCard state={state} run={run}/>
                <UsageCard state={state} run={run}/>
                <div className="flex gap-2">
                    <Button variant="outline" onClick={() => run('测试通知', TestNotify)} disabled={!state.connected}><Bell/>发测试通知</Button>
                    <Button variant="outline" onClick={() => setWizard(true)}><RefreshCw/>换手环 / 重新绑定</Button>
                </div>
                </div>
            </main>
            {toast && (
                <div className="fixed bottom-5 left-1/2 z-20 -translate-x-1/2 rounded-lg border bg-popover px-4 py-2 text-sm text-popover-foreground shadow-lg">
                    {toast}
                </div>
            )}
        </div>
    )
}

// ---------- 通用的设置行 ----------

// 一行设置：左边标题和说明，右边控件（开关之类）
function Row({title, hint, children}: { title: ReactNode, hint?: ReactNode, children?: ReactNode }) {
    return (
        <div className="flex items-center justify-between gap-6">
            <div className="min-w-0">
                <div className="text-sm font-medium">{title}</div>
                {hint && <div className="mt-0.5 text-xs text-muted-foreground">{hint}</div>}
            </div>
            {children && <div className="shrink-0">{children}</div>}
        </div>
    )
}

// 几个选项里选一个：选项少平铺成一排，多了用下拉框
function Choice({value, options, onChange, disabled}: {
    value: string, options: { value: string, label: string }[], onChange: (v: string) => void, disabled?: boolean,
}) {
    if (options.length > 4) {
        return (
            <Select value={value} onValueChange={onChange} disabled={disabled}>
                <SelectTrigger className="w-48"><SelectValue/></SelectTrigger>
                <SelectContent>
                    {options.map(o => <SelectItem key={o.value} value={o.value}>{o.label}</SelectItem>)}
                </SelectContent>
            </Select>
        )
    }
    return (
        <ToggleGroup type="single" variant="outline" size="sm" value={value} disabled={disabled}
                     onValueChange={v => v && v !== value && onChange(v)}>
            {options.map(o => (
                <ToggleGroupItem key={o.value} value={o.value}
                                 className="px-3 data-[state=on]:border-primary data-[state=on]:bg-primary data-[state=on]:text-primary-foreground">
                    {o.label}
                </ToggleGroupItem>
            ))}
        </ToggleGroup>
    )
}

// ---------- 状态 ----------

function StatusBadge({state}: { state: AppState }) {
    let text = '手环已连接'
    let dot = 'bg-emerald-500'
    if (!state.connected) {
        text = state.lastError ? '连接失败，重试中' : '正在连接手环…'
        dot = state.lastError ? 'bg-red-500' : 'bg-amber-500'
    }
    const battery = state.connected ? state.battery : null
    return (
        <Badge variant="outline" title={state.lastError || (battery?.charging ? '充电中' : undefined)} className="gap-1.5">
            <span className={`size-1.5 rounded-full ${dot}`}/>{text}
            {battery && <span className="flex items-center text-muted-foreground">
                · {battery.charging && <Zap className="size-3"/>}{battery.level}%
            </span>}
        </Badge>
    )
}

// ---------- 选中 app 的配置 ----------

function AppSettingsCard({app, run}: { app: AppInfo, run: Run }) {
    return (
        <Card>
            <CardHeader>
                <CardTitle className="flex items-center gap-2"><LayoutGrid className="size-4"/>{app.title}</CardTitle>
                <CardDescription>左边预览的这个 app 的设置，改了马上推到手环</CardDescription>
            </CardHeader>
            <CardContent className="flex flex-col gap-5">
                {(app.settings ?? []).map(s => <SettingField key={s.key} app={app} setting={s} run={run}/>)}
            </CardContent>
        </Card>
    )
}

// app 的一个配置项：照着 manifest.json 里 settings 的声明画
function SettingField({app, setting, run}: { app: AppInfo, setting: AppSetting, run: Run }) {
    const value = app.config?.[setting.key] ?? setting.default
    const save = (v: unknown) => run('保存配置', () => SetAppConfig(app.name, setting.key, JSON.stringify(v)))
    switch (setting.type) {
        case 'bool':
            return <Row title={setting.title} hint={setting.description}><Switch checked={!!value} onCheckedChange={save}/></Row>
        case 'select':
            return (
                <div className="flex flex-col gap-2">
                    <Row title={setting.title} hint={setting.description}/>
                    <Choice value={String(value)} options={setting.options ?? []} onChange={save}/>
                </div>
            )
        default:
            return (
                <div className="flex flex-col gap-2">
                    <Row title={setting.title} hint={setting.description}/>
                    <Input type={setting.type === 'number' ? 'number' : 'text'} defaultValue={String(value ?? '')} className="max-w-64"
                           onBlur={e => {
                               if (e.target.value === String(value)) return
                               save(setting.type === 'number' ? Number(e.target.value) : e.target.value)
                           }}/>
                </div>
            )
    }
}

// ---------- 菜单栏的猫：App 自己的设置，和手环上 Agent 猫的「猫」分开选 ----------

function MenubarCard({state, run}: { state: AppState, run: Run }) {
    const current = state.settings?.pet ?? 'orange'
    return (
        <Card>
            <CardHeader>
                <CardTitle className="flex items-center gap-2"><PawPrint className="size-4"/>菜单栏的猫</CardTitle>
                <CardDescription>手环上的猫在 Agent 猫的设置里单独选</CardDescription>
            </CardHeader>
            <CardContent className="flex gap-2">
                {SKINS.map(s => (
                    <Button key={s.value} variant={s.value === current ? 'secondary' : 'ghost'}
                            className={`h-auto flex-col gap-1 px-4 py-2 ${s.value === current ? 'ring-2 ring-ring' : ''}`}
                            onClick={() => s.value !== current && run('换猫', () => SetPet(s.value as Skin))}>
                        <img src={`/pet/${s.value}/sleep_0.png`} width={40} height={40} alt="" className="pixel"/>
                        <span className="text-xs">{s.label}</span>
                    </Button>
                ))}
            </CardContent>
        </Card>
    )
}

// ---------- Claude Code 额度：app 读额度要的数据，得由 App 改 Claude Code 的设置才拿得到 ----------

function UsageCard({state, run}: { state: AppState, run: Run }) {
    const s = state.settings
    if (!s) return null
    return (
        <Card>
            <CardHeader>
                <CardTitle className="flex items-center gap-2"><Gauge className="size-4"/>Claude Code 额度</CardTitle>
            </CardHeader>
            <CardContent>
                <Row title="让 app 能显示 Claude Code 额度"
                     hint="Claude Code 只把额度告诉状态栏命令，所以要把它的状态栏换成一个小脚本：先存一份数据，再交给你原来的状态栏命令，显示不变。关掉会原样还原（改之前会备份 ~/.claude/settings.json）。只有 Pro/Max 订阅才有额度；Codex 的额度不用设置。">
                    <Switch checked={s.claudeUsage} onCheckedChange={v => run(v ? '打开额度' : '关掉额度', () => SetClaudeUsage(v))}/>
                </Row>
            </CardContent>
        </Card>
    )
}

// ---------- app 列表：装到手环、更新、权限；以前版本留在手环上的旧应用 ----------

const LEGACY_NAMES: Record<string, string> = {
    'dev.bysir.herdrpet': 'Agent 猫',
    'dev.bysir.herdrband': 'Agents',
    'dev.bysir.mibandpet': 'wristkit',
}

function AppsCard({state, run, onSelect}: { state: AppState, run: Run, onSelect: (name: string) => void }) {
    const list = state.apps ?? []
    const off = !state.connected || !!state.installing
    const [newer, setNewer] = useState<Record<string, boolean>>({}) // 从 GitHub 下的 app：检查过有没有新版本
    const [deleting, setDeleting] = useState('') // 点了「删除」，等再点一下确认
    const check = (name: string) => run('检查新版本', async () => {
        const has = await CheckAppUpdate(name)
        setNewer(m => ({...m, [name]: has}))
    })
    const remove = (a: AppInfo, installed: boolean) => run('删除', async () => {
        setDeleting('')
        if (installed && state.connected) await Uninstall(a.package)
        await DeleteApp(a.name)
    })
    return (
        <Card>
            <CardHeader>
                <CardTitle className="flex items-center gap-2"><Watch className="size-4"/>App <span className="text-muted-foreground">{list.length}</span></CardTitle>
                <CardDescription>每个 app 在手环上是一个单独的应用。自己写 app：把文件夹放进 app 目录，改了左边预览马上更新，装到手环上要点一下</CardDescription>
            </CardHeader>
            <CardContent className="flex flex-col gap-4">
                {list.map(a => {
                    const st = bandStatus(state, a)
                    const busy = state.installing === a.name
                    const needInstall = !st.installed || st.outdated
                    const install = (variant: 'default' | 'ghost') => (
                        <Button size="sm" variant={variant} disabled={off || !!a.error}
                                onClick={() => run(!st.installed ? '安装' : '更新', () => InstallApp(a.name))}>
                            {busy ? `${Math.round(state.installProgress * 100)}%` : !st.installed ? '装到手环' : st.outdated ? '更新' : '重新安装'}
                        </Button>
                    )
                    return (
                        <div key={a.name} className="flex flex-col gap-2 border-t pt-4 first:border-t-0 first:pt-0">
                            <div className="min-w-0">
                                <div className="flex flex-wrap items-center gap-2 text-sm font-medium">
                                    {a.title}{a.foreground && <Badge variant="secondary">手环上开着</Badge>}
                                </div>
                                <div className="mt-0.5 text-xs text-muted-foreground">
                                    {a.source ? <SourceLink url={a.source}/> : a.builtin ? '内置' : shortPath(a.dir)}{' · ' + st.text}
                                </div>
                            </div>
                            {/* 主要操作在前、有底色或描边；其余是无边框的次要操作 */}
                            <div className="flex flex-wrap items-center gap-1">
                                {a.needGrant && <Button size="sm" onClick={() => run('允许', () => AllowApp(a.name, a.permKey))}>允许</Button>}
                                {needInstall && install('default')}
                                {st.installed && <Button variant="outline" size="sm" disabled={off}
                                                         onClick={() => run('打开', () => OpenApp(a.name))}>在手环上打开</Button>}
                                <Button variant="ghost" size="sm" onClick={() => onSelect(a.name)}>预览</Button>
                                {!needInstall && install('ghost')}
                                {a.builtin && <Button variant="ghost" size="sm" title="把源码复制到 app 目录，改了就替换内置的这个"
                                                      onClick={() => run('复制', () => CopyBuiltin(a.name))}>复制出来改</Button>}
                                {a.source && (newer[a.name]
                                    ? <Button variant="outline" size="sm" title="从 GitHub 下载新版本，下好了再点「更新」装到手环"
                                              onClick={() => run('下载新版本', async () => {
                                                  await UpdateApp(a.name)
                                                  setNewer(m => ({...m, [a.name]: false}))
                                              })}>下载新版本</Button>
                                    : <Button variant="ghost" size="sm" onClick={() => check(a.name)}>{newer[a.name] === false ? '已是最新' : '检查新版本'}</Button>)}
                                {!a.builtin && !a.needGrant && (a.permissions?.length ?? 0) > 0 &&
                                    <Button variant="ghost" size="sm" onClick={() => run('收回权限', () => RevokeApp(a.name))}>收回权限</Button>}
                                {!a.builtin && (deleting === a.name
                                    ? <Button variant="destructive" size="sm" title={st.installed ? '手环上装的也一起卸载' : undefined}
                                              onClick={() => remove(a, st.installed)}>确定删除？</Button>
                                    : <Button variant="ghost" size="sm" title="移到废纸篓"
                                              onClick={() => setDeleting(a.name)}>删除</Button>)}
                            </div>
                            {busy && (
                                <div className="h-1 overflow-hidden rounded-full bg-muted">
                                    <div className="h-full bg-primary transition-all" style={{width: `${state.installProgress * 100}%`}}/>
                                </div>
                            )}
                            {!a.builtin && (a.permissions?.length ?? 0) > 0 && (
                                <p className="text-xs text-muted-foreground">
                                    需要权限：{a.permissions!.join('、')}{a.needGrant ? '（还没允许，这部分功能用不了）' : ''}
                                </p>
                            )}
                            {a.error && <p className="max-h-32 overflow-auto whitespace-pre-wrap break-all text-xs text-destructive">{a.error}</p>}
                        </div>
                    )
                })}
                {(state.legacy ?? []).map(pkg => (
                    <div key={pkg} className="border-t pt-4 first:border-t-0 first:pt-0">
                    <Row title={<>{LEGACY_NAMES[pkg] ?? pkg} <span className="text-muted-foreground">（旧版）</span></>}
                         hint="以前版本装的，已经不用了，可以卸载">
                        <div className="flex gap-2">
                            <Button variant="outline" size="sm" disabled={off} onClick={() => run('打开', () => Launch(pkg))}>在手环上打开</Button>
                            <Button variant="destructive" size="sm" disabled={off} onClick={() => run('卸载', () => Uninstall(pkg))}>卸载</Button>
                        </div>
                    </Row>
                    </div>
                ))}
                <div className="border-t pt-4 first:border-t-0 first:pt-0"><DownloadRow run={run}/></div>
                <div>
                    <Button variant="outline" size="sm" onClick={() => run('打开 app 目录', OpenAppDir)}><FolderOpen/>打开 app 目录</Button>
                </div>
            </CardContent>
        </Card>
    )
}

// 从 GitHub 下载 app：贴 app 目录的链接，或者 owner/仓库/目录
function DownloadRow({run}: { run: Run }) {
    const [input, setInput] = useState('')
    const [busy, setBusy] = useState(false)
    const go = () => {
        if (!input.trim() || busy) return
        setBusy(true)
        run('下载', () => DownloadApp(input.trim()).then(() => setInput('')).finally(() => setBusy(false)))
    }
    return (
        <div className="flex flex-col gap-1.5">
            <div className="flex gap-2">
                <Input value={input} placeholder="从 GitHub 下载：贴 app 目录的链接，或者 owner/仓库/目录"
                       onChange={e => setInput(e.target.value)} onKeyDown={e => e.key === 'Enter' && go()}/>
                <Button size="sm" disabled={busy || !input.trim()} onClick={go}><Download/>{busy ? '下载中' : '下载'}</Button>
            </div>
            <p className="text-xs text-muted-foreground">app 会在手环和这台 Mac 上运行，只装你信任的来源</p>
        </div>
    )
}

// 来源链接显示成 owner/仓库/目录，点了在浏览器打开
function SourceLink({url}: { url: string }) {
    const m = url.match(/^https:\/\/github\.com\/([^/]+\/[^/]+)(?:\/tree\/([^/]+)\/?(.*))?$/)
    const text = m ? m[1] + (m[3] ? '/' + m[3] : '') + (m[2] && m[2] !== 'HEAD' ? '@' + m[2] : '') : url
    return <a className="cursor-pointer underline-offset-2 hover:underline" onClick={() => BrowserOpenURL(url)}>{text}</a>
}

function shortPath(p: string): string {
    return p.replace(/^\/Users\/[^/]+/, '~')
}
