// 在 App 窗口里跑 app：跑的是装到手环上的同一份代码（Go 编好的 app.js 和页面 pages/index/index.js），
// 这里模拟手环上的快应用框架：
//   - 页面的数据变成响应式（@vue/reactivity），模板里每个用到数据的值（aiot.__ce__ 等调用里的函数）各是一个 effect，
//     数据变了只更新用到它的那几处，和手环上一样
//   - 元素画成 HTML：div 只有 flex 布局、默认横排；text 是文字；image 是图片；image-animator 是逐帧动画；swiper 是能拖动的翻页
//   - 快应用的接口（@system.interconnect、storage、device、vibrator）由 host 提供：interconnect 接到 Mac（Wails）
import {effect, effectScope, getCurrentScope, onScopeDispose, pauseTracking, reactive, resetTracking} from '@vue/reactivity'
import type {EffectScope} from '@vue/reactivity'

export interface Bundle {
    app: string // 编好的 app.js
    page: string // 编好的页面
    assets: Record<string, string> // 图片：/assets/a.png → data URL
}

export interface Host {
    send(m: any): void // app 发给 Mac 的消息（hello、bye、call、log）
    vibrate(): void
    log(level: string, msg: string): void
    storageKey: string // localStorage 的前缀
    width: number
    height: number
}

export interface Running {
    recv(m: any): void // Mac 发来的消息
    show(): void
    hide(): void
    destroy(): void
}

type Desc =
    | { k: 'el', tag: string, opts: Record<string, any>, children: Desc[] }
    | { k: 'if', shown: () => any, fn: () => Desc[] }
    | { k: 'for', exp: () => any, fn: (i: number, item: any) => Desc[] }

const aiot = {
    __ce__: (tag: string, o: any, children?: Desc[]): Desc => ({k: 'el', tag, opts: o.__opts__ || {}, children: children || []}),
    __ci__: (o: any, fn: () => Desc[]): Desc => ({k: 'if', shown: o.__opts__.shown, fn}),
    __cf__: (o: any, fn: (i: number, item: any) => Desc[]): Desc => ({k: 'for', exp: o.__opts__.exp, fn}),
}

// 编好的模块是 `export default function (global, globalThis, window, $app_exports$, $app_evaluate$) {…}`
function loadModule(src: string, require: (name: string) => any, console: any) {
    const prefix = 'export default function'
    if (!src.startsWith(prefix)) throw new Error('编好的代码格式不对')
    return new Function('$app_require$', 'aiot', 'console', 'return function' + src.slice(prefix.length))(require, aiot, console)
}

const val = (v: any) => (typeof v === 'function' ? v() : v)

// ---------- 样式 ----------

type StyleRule = [[[number, string]], Record<string, any>]

// 数字值要不要加 px
const UNITLESS = new Set(['flex', 'flexGrow', 'flexShrink', 'opacity', 'fontWeight', 'zIndex', 'lines'])

// 手环快应用的布局默认值：只有 flex，div 默认横排，边框写了宽度和颜色就是实线
const BOX_BASE = 'display:flex;flex-direction:row;box-sizing:border-box;position:relative;min-width:0;min-height:0;border-style:solid;border-width:0;flex-shrink:1;'
// 文字默认不收缩：手环上一行放不下时不会把文字压窄折行（浏览器会，比如「54」被拆成两行）；app 写了 flex、宽度的照写的来
const TEXT_BASE = 'display:block;box-sizing:border-box;border-style:solid;border-width:0;flex-shrink:0;min-width:0;white-space:pre-wrap;word-break:break-all;color:#ffffff;font-size:14px;'

function kebab(k: string) {
    return k.replace(/[A-Z]/g, c => '-' + c.toLowerCase())
}

function cssText(tag: string, s: Record<string, any>): string {
    let out = tag === 'text' || tag === 'span' || tag === 'a' ? TEXT_BASE : tag === 'image' || tag === 'image-animator' ? 'box-sizing:border-box;object-fit:cover;flex-shrink:1;' : BOX_BASE
    if (tag === 'span' || tag === 'a') out += 'display:inline;'
    for (const k in s) {
        const v = s[k]
        if (v === undefined || v === null || v === '') continue
        if (k === 'lines') {
            const n = Number(v)
            if (n === 1) out += 'white-space:nowrap;overflow:hidden;'
            else if (n > 1) out += `display:-webkit-box;-webkit-line-clamp:${n};-webkit-box-orient:vertical;overflow:hidden;`
            continue
        }
        out += kebab(k) + ':' + (typeof v === 'number' && !UNITLESS.has(k) ? v + 'px' : v) + ';'
    }
    return out
}

// 按选择器合并样式：标签 < class < id < 写在元素上的
function computeStyle(rules: StyleRule[], tag: string, classes: string[], id: string | undefined, inline: Record<string, any> | undefined) {
    const out: Record<string, any> = {}
    for (const kind of [2, 0, 1]) {
        for (const [[sel], decl] of rules) {
            const [k, name] = sel
            if (k !== kind) continue
            if ((k === 2 && name === tag) || (k === 0 && classes.includes(name)) || (k === 1 && name === id)) Object.assign(out, decl)
        }
    }
    if (inline) Object.assign(out, inline)
    return out
}

function classList(v: any): string[] {
    const c = val(v)
    if (Array.isArray(c)) return c
    if (typeof c === 'string') return c.split(' ').map(s => s.trim()).filter(Boolean)
    return []
}

// ---------- 运行 ----------

export function run(container: HTMLElement, bundle: Bundle, host: Host): Running {
    const timers = new Set<any>()
    const intervals = new Set<any>()
    const g: any = {
        setTimeout: (fn: any, ms?: number, ...a: any[]) => {
            const t = setTimeout(() => {
                timers.delete(t)
                fn(...a)
            }, ms)
            timers.add(t)
            return t
        },
        clearTimeout: (t: any) => {
            timers.delete(t)
            clearTimeout(t)
        },
        setInterval: (fn: any, ms?: number, ...a: any[]) => {
            const t = setInterval(fn, ms, ...a)
            intervals.add(t)
            return t
        },
        clearInterval: (t: any) => {
            intervals.delete(t)
            clearInterval(t)
        },
    }
    const fakeConsole = {
        log: (...a: any[]) => host.log('log', a.map(String).join(' ')),
        info: (...a: any[]) => host.log('log', a.map(String).join(' ')),
        warn: (...a: any[]) => host.log('warn', a.map(String).join(' ')),
        error: (...a: any[]) => host.log('error', a.map(String).join(' ')),
    }
    const conn: any = {
        send: (o: any) => {
            host.send(o.data)
            o.success?.()
        },
    }
    const require = (name: string) => {
        switch (name) {
            case '@app-module/system.interconnect':
                return {instance: () => conn}
            case '@app-module/system.storage':
                return {
                    get: (o: any) => {
                        const v = localStorage.getItem(host.storageKey + o.key)
                        if (v == null) o.fail?.('', 202)
                        else o.success?.(v)
                    },
                    set: (o: any) => {
                        try {
                            localStorage.setItem(host.storageKey + o.key, String(o.value))
                        } catch { /* 预览里存不了就算了 */ }
                        o.success?.()
                    },
                    delete: (o: any) => {
                        localStorage.removeItem(host.storageKey + o.key)
                        o.success?.()
                    },
                }
            case '@app-module/system.device':
                return {getInfo: (o: any) => o.success?.({screenWidth: host.width, screenHeight: host.height, product: 'wristkit preview'})}
            case '@app-module/system.vibrator':
                return {vibrate: () => host.vibrate()}
        }
        return {}
    }

    // app.js 设置 $translateStyle$（动态 style 用），页面在 $app_exports$.entry 里登记自己
    const appExports: any = {}
    loadModule(bundle.app, require, fakeConsole)(g, g, g, appExports)
    const pageExports: any = {}
    loadModule(bundle.page, require, fakeConsole)(g, g, g, pageExports)
    pageExports.entry(pageExports)
    const def = pageExports.default
    const rules: StyleRule[] = def.style || []

    const elements: Record<string, any> = {}
    const vm: any = reactive(JSON.parse(JSON.stringify(def.data || {})))
    for (const k of Object.keys(def)) {
        if (typeof def[k] === 'function' && k !== 'template') vm[k] = def[k]
    }
    vm.$app = {exit: () => {}, $def: appExports.default}
    vm.$element = (id: string) => elements[id]

    const scope = effectScope()
    container.innerHTML = ''
    scope.run(() => {
        const root = def.template(vm)
        mount([root], container, null)
    })

    function mount(list: Desc[], parent: Node, before: Node | null) {
        for (const d of list) mountOne(d, parent, before)
    }

    // 在一个新的 effect 作用域里挂（if / for 的分支），返回拆掉它的函数
    function mountBranch(list: Desc[], parent: Node, before: Node) {
        // 首尾各放一个标记，拆的时候只删这两个标记之间的（for 的几项挨着放，不能删到别的项）
        const s: EffectScope = effectScope(true)
        const start = document.createComment('')
        const end = document.createComment('')
        parent.insertBefore(start, before)
        parent.insertBefore(end, before)
        pauseTracking()
        try {
            s.run(() => mount(list, parent, end))
        } finally {
            resetTracking()
        }
        return () => {
            s.stop()
            let n: Node | null = start
            while (n) {
                const next: Node | null = n === end ? null : n.nextSibling
                parent.removeChild(n)
                n = next
            }
        }
    }

    function mountOne(d: Desc, parent: Node, before: Node | null) {
        if (d.k === 'if') {
            const anchor = document.createComment('if')
            parent.insertBefore(anchor, before)
            let shown: boolean | null = null
            let drop: (() => void) | null = null
            effect(() => {
                const s = !!d.shown()
                if (s === shown) return
                shown = s
                drop?.()
                drop = s ? mountBranch(d.fn(), parent, anchor) : null
            })
            onScopeDispose(() => drop?.())
            return
        }
        if (d.k === 'for') {
            const anchor = document.createComment('for')
            parent.insertBefore(anchor, before)
            let drops: (() => void)[] = []
            effect(() => {
                const e = d.exp()
                const list = e && e.__list__ !== undefined ? e.__list__ : e
                const items: any[] = []
                if (list) for (let i = 0; i < list.length; i++) items.push(list[i])
                drops.forEach(f => f())
                drops = items.map((item, i) => mountBranch(d.fn(i, item), parent, anchor))
            })
            onScopeDispose(() => drops.forEach(f => f()))
            return
        }
        const el = createElement(d)
        parent.insertBefore(el, before)
        mount(d.children, el, null)
    }

    function createElement(d: Extract<Desc, { k: 'el' }>): HTMLElement {
        const {tag, opts} = d
        const img = tag === 'image' || tag === 'image-animator'
        const el = document.createElement(img ? 'img' : tag === 'span' || tag === 'a' ? 'span' : 'div')
        if (img) {
            (el as HTMLImageElement).draggable = false
            el.className = 'pixel'
        }
        // 样式：class、id、style 都可能变，合起来算
        effect(() => {
            const st = computeStyle(rules, tag, classList(opts.classList), val(opts.id), val(opts.style))
            let css = cssText(tag, st)
            if (opts.show !== undefined && !val(opts.show)) css += 'display:none;'
            if (opts.events?.click) css += 'cursor:pointer;'
            el.style.cssText = css
        })
        if (opts.value !== undefined && (tag === 'text' || tag === 'span' || tag === 'a')) {
            effect(() => {
                const v = val(opts.value)
                el.textContent = v == null ? '' : String(v)
            })
        }
        if (opts.src !== undefined) {
            effect(() => {
                const src = String(val(opts.src) ?? '')
                ;(el as HTMLImageElement).src = bundle.assets[src] ?? src
            })
        }
        if (tag === 'swiper') return swiper(el, d)
        if (tag === 'image-animator') animator(el as HTMLImageElement, d)
        const ev = opts.events || {}
        if (ev.click) {
            el.addEventListener('click', e => {
                e.stopPropagation() // 里外两层都能点时只算最里面的
                ev.click({type: 'click'})
            })
        }
        if (ev.longpress) {
            el.addEventListener('contextmenu', e => {
                e.preventDefault()
                e.stopPropagation()
                ev.longpress({type: 'longpress'})
            })
        }
        if (ev.swipe) swipeGesture(el, dir => ev.swipe({type: 'swipe', direction: dir}))
        const id = val(opts.id)
        if (id && !elements[id]) elements[id] = {}
        return el
    }

    // swiper：按住拖动时页面跟着走，松手按拖动距离吸附到上一页或下一页；index 变了就翻过去
    function swiper(el: HTMLElement, d: Extract<Desc, { k: 'el' }>): HTMLElement {
        const {opts} = d
        const track = document.createElement('div')
        el.appendChild(track)
        let index = 0
        let drag: number | null = null
        const vertical = () => String(val(opts.vertical)) !== 'false'
        const size = () => (vertical() ? el.clientHeight : el.clientWidth)
        const count = () => track.children.length
        const layout = () => {
            const v = vertical()
            track.style.cssText = `display:flex;flex-direction:${v ? 'column' : 'row'};flex-shrink:0;align-self:flex-start;` +
                `transform:translate${v ? 'Y' : 'X'}(${-index * size() + (drag ?? 0)}px);transition:${drag == null ? 'transform .28s ease-out' : 'none'};`
            for (const c of Array.from(track.children) as HTMLElement[]) {
                c.style.flexShrink = '0'
                c.style.width = el.clientWidth + 'px'
                c.style.height = el.clientHeight + 'px'
            }
        }
        const goTo = (i: number) => {
            const to = Math.max(0, Math.min(count() - 1, i))
            if (to === index) return layout()
            index = to
            layout()
            opts.events?.change?.({type: 'change', index: to})
        }
        el.style.overflow = 'hidden'
        effect(() => {
            el.style.overflow = 'hidden'
            el.style.touchAction = 'none'
        })
        effect(() => {
            const ix = Number(val(opts.index) ?? 0)
            if (!Number.isNaN(ix)) {
                index = ix
                requestAnimationFrame(layout)
            }
        })
        const id = val(opts.id)
        if (id) elements[id] = {swipeTo: (o: { index: number }) => goTo(o.index)}
        let start: { at: number, moved: boolean } | null = null
        const at = (e: PointerEvent) => (vertical() ? e.clientY : e.clientX) / zoomOf(el)
        el.addEventListener('pointerdown', e => {
            start = {at: at(e), moved: false}
        })
        el.addEventListener('pointermove', e => {
            if (!start) return
            const dist = at(e) - start.at
            if (!start.moved && Math.abs(dist) > 4) {
                start.moved = true
                el.setPointerCapture(e.pointerId)
            }
            if (start.moved) {
                drag = dist
                layout()
            }
        })
        const up = () => {
            const dist = drag ?? 0
            drag = null
            if (Math.abs(dist) > size() * 0.2) goTo(index + (dist < 0 ? 1 : -1))
            else layout()
            setTimeout(() => {
                start = null
            }, 0)
        }
        el.addEventListener('pointerup', up)
        el.addEventListener('pointercancel', up)
        // 拖动过的那一下不算点击
        el.addEventListener('click', e => {
            if (start?.moved) e.stopPropagation()
        }, true)
        // 页面挂进 track 里（等 swiper 自己放进页面、有了大小再挂）
        const pages = d.children
        d.children = []
        const owner = getCurrentScope() ?? scope
        queueMicrotask(() => {
            pauseTracking()
            try {
                owner.run(() => mount(pages, track, null))
            } finally {
                resetTracking()
            }
            layout()
        })
        return el
    }

    // image-animator：images 是帧列表（[{src}]），duration 是每帧停多久；start / stop 由页面调（$element(id)）。
    // 帧列表或时长变了从第一帧重新播
    function animator(el: HTMLImageElement, d: Extract<Desc, { k: 'el' }>) {
        const {opts} = d
        let timer: any = null
        let playing = true
        let frames: string[] = []
        let ms = 1000
        let i = 0
        const show = () => {
            const src = frames[i % Math.max(1, frames.length)]
            if (src) el.src = bundle.assets[src] ?? src
        }
        const restart = () => {
            g.clearInterval(timer)
            i = 0
            show()
            if (playing && frames.length > 1) {
                timer = g.setInterval(() => {
                    i = (i + 1) % frames.length
                    show()
                }, ms)
            }
        }
        effect(() => {
            const list = val(opts.images) || []
            frames = Array.from(list as any[], (f: any) => String(f?.src ?? ''))
            const v = val(opts.duration)
            ms = Math.max(30, parseFloat(String(v ?? '1000')) * (/[^m]s$/.test(String(v)) ? 1000 : 1) || 1000)
            restart()
        })
        onScopeDispose(() => g.clearInterval(timer))
        const id = val(opts.id)
        if (id) {
            elements[id] = {
                start: () => {
                    playing = true
                    restart()
                },
                stop: () => {
                    playing = false
                    restart()
                },
                pause: () => {
                    playing = false
                    g.clearInterval(timer)
                },
                resume: () => {
                    playing = true
                    restart()
                },
            }
        }
    }

    function swipeGesture(el: HTMLElement, fire: (dir: string) => void) {
        let start: { x: number, y: number } | null = null
        el.addEventListener('pointerdown', e => {
            start = {x: e.clientX, y: e.clientY}
        })
        el.addEventListener('pointerup', e => {
            if (!start) return
            const dx = e.clientX - start.x
            const dy = e.clientY - start.y
            start = null
            if (Math.abs(dy) > 30 && Math.abs(dy) > Math.abs(dx)) fire(dy < 0 ? 'up' : 'down')
            else if (Math.abs(dx) > 30) fire(dx < 0 ? 'left' : 'right')
        })
    }

    let alive = true
    const call = (name: string, ...args: any[]) => {
        if (!alive || typeof vm[name] !== 'function') return
        try {
            vm[name](...args)
        } catch (e) {
            host.log('error', `${name}：${e instanceof Error ? e.stack || e.message : e}`)
        }
    }
    call('onInit')
    call('onReady')
    let visible = false
    const show = () => {
        if (visible || !alive) return
        visible = true
        call('onShow')
        conn.onopen?.()
    }
    return {
        recv: (m: any) => {
            if (alive) conn.onmessage?.({data: m})
        },
        show,
        hide: () => {
            if (!visible || !alive) return
            visible = false
            call('onHide')
        },
        destroy: () => {
            if (!alive) return
            if (visible) call('onHide')
            call('onDestroy')
            alive = false
            scope.stop()
            timers.forEach(t => clearTimeout(t))
            intervals.forEach(t => clearInterval(t))
            container.innerHTML = ''
        },
    }
}

// 元素所在的缩放（预览屏幕用 CSS zoom 放大）
function zoomOf(el: HTMLElement): number {
    let z = 1
    for (let n: HTMLElement | null = el; n; n = n.parentElement) {
        const v = parseFloat((n.style as any).zoom)
        if (v) z *= v
    }
    return z
}
