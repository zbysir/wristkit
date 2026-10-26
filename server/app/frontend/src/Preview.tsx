import {useEffect, useRef, useState} from 'react'
import type {CSSProperties, RefObject} from 'react'
import {Download, Watch} from 'lucide-react'
import {Button} from '@/components/ui/button'
import {Tabs, TabsList, TabsTrigger} from '@/components/ui/tabs'
import {AppCall, AppPreview, AppStores, InstallApp, OpenApp} from '../wailsjs/go/main/App'
import {EventsOn} from '../wailsjs/runtime/runtime'
import {run as runApp} from './uxrt'
import type {Running} from './uxrt'
import type {AppInfo, AppState} from './types'
import {bandStatus} from './status'

// 在 Mac 窗口里跑 app：跑的是装到手环上的同一份代码（见 uxrt.ts），数据也是 Mac 推给手环的同一份。
// 点击、长按（右键）、拖动翻页都能在这里调试，call() 也会交给 Mac 上真正的 server.js。

const SCREEN = {width: 212, height: 303}
const ZOOM = 1.2

// 预览看不看得见：窗口藏到菜单栏、最小化、被别的窗口整个挡住时页面是 hidden；屏幕区域滚出视野也算看不见
function useVisible(ref: RefObject<HTMLElement | null>) {
    const [pageVisible, setPageVisible] = useState(!document.hidden)
    const [inView, setInView] = useState(true)
    useEffect(() => {
        const onChange = () => setPageVisible(!document.hidden)
        document.addEventListener('visibilitychange', onChange)
        return () => document.removeEventListener('visibilitychange', onChange)
    }, [])
    useEffect(() => {
        if (!ref.current) return
        const io = new IntersectionObserver(([e]) => setInView(e.isIntersecting))
        io.observe(ref.current)
        return () => io.disconnect()
    }, [ref])
    return pageVisible && inView
}

// 把 app 跑在 screen 里：app 发 hello 后把 Mac 上它的数据推进去，之后数据变了再推；看不见时当成 app 退到后台
function useApp(app: AppInfo | undefined, screen: RefObject<HTMLDivElement | null>, visible: boolean) {
    const [error, setError] = useState('')
    const [flash, setFlash] = useState(false)
    const running = useRef<Running | null>(null)

    useEffect(() => {
        setError('')
        if (!app || !screen.current) return
        if (app.error && !app.version) {
            setError(app.error)
            return
        }
        const name = app.name
        const el = screen.current
        let alive = true
        let listening = false
        let last: Record<string, string> = {}
        const push = () => {
            if (!alive || !listening) return
            AppStores().then(json => {
                if (!alive || !listening) return
                const stores = JSON.parse(json) as Record<string, unknown>
                const prefix = name + ':'
                for (const [k, v] of Object.entries(stores)) {
                    if (!k.startsWith(prefix)) continue
                    const s = JSON.stringify(v)
                    if (last[k] === s) continue
                    last[k] = s
                    running.current?.recv({t: 'store', name: k.slice(prefix.length), v})
                }
            })
        }
        const off = EventsOn('stores', push)
        AppPreview(name).then(bundle => {
            if (!alive || !bundle) return
            try {
                running.current = runApp(el, bundle, {
                    width: SCREEN.width,
                    height: SCREEN.height,
                    storageKey: `preview:${name}:`,
                    vibrate: () => {
                        setFlash(true)
                        setTimeout(() => setFlash(false), 300)
                    },
                    log: (level, msg) => {
                        console.log(`[${name}] ${level}`, msg)
                        if (level === 'error') setError(msg)
                    },
                    send: m => {
                        switch (m?.t) {
                            case 'hello':
                                listening = true
                                last = {}
                                push()
                                break
                            case 'bye':
                                listening = false
                                break
                            case 'call':
                                AppCall(name, m.name, JSON.stringify(m.args ?? [])).catch(err => setError(`call('${m.name}') 失败：${err}`))
                                break
                            case 'log':
                                console.log(`[${name}] ${m.level}`, m.msg)
                                if (m.level === 'error') setError(m.msg)
                                break
                        }
                    },
                })
                if (!document.hidden) running.current.show()
            } catch (err) {
                setError('加载出错：' + (err instanceof Error ? err.message : String(err)))
            }
        }).catch(err => alive && setError(String(err)))
        return () => {
            alive = false
            off()
            running.current?.destroy()
            running.current = null
        }
    }, [app?.name, app?.version])

    useEffect(() => {
        if (visible) running.current?.show()
        else running.current?.hide()
    }, [visible])

    return {error, flash}
}

// 左边的手环预览。选哪个 app 由 App 管（右边的设置跟着选中的 app 走）
export function Preview({state, run, app, onSelect}: {
    state: AppState, run: (label: string, fn: () => Promise<unknown>) => void,
    app: AppInfo | undefined, onSelect: (name: string) => void,
}) {
    const apps = state.apps ?? []
    const screenRef = useRef<HTMLDivElement>(null)
    const visible = useVisible(screenRef)
    const {error, flash} = useApp(app, screenRef, visible)
    const st = app ? bandStatus(state, app) : null

    return (
        <div className="flex flex-col items-center gap-4">
            {apps.length > 1 && (
                <Tabs value={app?.name} onValueChange={onSelect}>
                    <TabsList>
                        {apps.map(a => <TabsTrigger key={a.name} value={a.name}>{a.title}</TabsTrigger>)}
                    </TabsList>
                </Tabs>
            )}
            {/* 画个手环：机身里是屏幕 */}
            <div className="band">
                <div className="band-body">
                    <div className={`preview-screen ${flash ? 'flash' : ''}`} style={{width: SCREEN.width * ZOOM, height: SCREEN.height * ZOOM}}
                         onDragStart={e => e.preventDefault()}>
                        <div ref={screenRef}
                             style={{width: SCREEN.width, height: SCREEN.height, zoom: ZOOM, overflow: 'hidden', background: '#000', color: '#fff', display: 'flex'} as CSSProperties}/>
                    </div>
                </div>
            </div>
            {error && <p className="w-full whitespace-pre-wrap break-all text-xs text-destructive">{error}</p>}
            {app && st && (
                <div className="flex w-full gap-2">
                    {st.installed && !st.outdated ? (
                        <Button className="flex-1" disabled={!state.connected || app.foreground}
                                onClick={() => run('打开', () => OpenApp(app.name))}>
                            <Watch/>{app.foreground ? '手环上正开着' : '在手环上打开'}
                        </Button>
                    ) : (
                        <Button className="flex-1" disabled={!state.connected || !!state.installing || !!app.buildError}
                                onClick={() => run(st.installed ? '更新' : '安装', () => InstallApp(app.name))}>
                            <Download/>{state.installing === app.name ? `${Math.round(state.installProgress * 100)}%` : st.installed ? '更新到手环' : '装到手环'}
                        </Button>
                    )}
                </div>
            )}
            <p className="text-center text-xs text-muted-foreground">
                和手环上跑的是同一份代码和数据：可以点，按住拖动翻页，右键当长按。改了 app 的文件这里马上更新，装到手环上要点一下。
            </p>
        </div>
    )
}
