// Agent 猫的 Mac 端（server.js 引入）：每 3 秒读一次 herdr 的 agent 和 gerdr 的会话（gerdr.js），合起来把 agent 状态推给手环；
// 数每天完成了几个任务（小鱼干），agent 跑完、卡住时提醒，卡住时可以保持手环亮屏。
// herdr、gerdr 都没装的用户：agent 列表是空的，猫一直睡觉，不报错。
import { publish, exec, storage, getConfig, onConfig, notifyBand, wakeBand, readFile, home } from 'miband/server'
import { listSessions } from './gerdr'

const POLL_MS = 3000
const MISSING_RETRY_MS = 30000 // 没装 herdr / gerdr、没开时，隔这么久再试，别一直试
const STATUS_TEXT = { done: '完成', idle: '完成', blocked: '等你处理', working: '开始跑' }

let config = getConfig()
onConfig((c) => (config = c))

// 提醒方式：notify 发通知（手环震动）/ wake 只亮屏（把手环点亮、切到猫，不震动）/ off 不提醒
const alertMode = () => config.alert || 'notify'
const impatientMin = () => (config.impatientMin == null ? 5 : config.impatientMin)

// ---------- 读 herdr、gerdr ----------

const project = (cwd) => (cwd || '').replace(/\/+$/, '').split('/').pop()

// 返回 agent 列表；没装 herdr、herdr 没在跑时返回 null
function listHerdr() {
  let r
  try {
    r = exec('herdr', ['agent', 'list'])
  } catch (e) {
    return null // 没找到命令
  }
  if (r.code !== 0) {
    console.warn('herdr agent list 失败：' + r.stderr.trim())
    return null
  }
  try {
    const agents = JSON.parse(r.stdout).result.agents || []
    // seq：herdr 里状态变化的序号，越大越近（排序用：同一档里最近变过状态的在前）
    return agents.map((a) => ({ id: a.pane_id, agent: a.agent, project: project(a.cwd), title: a.name || a.terminal_title_stripped || '', status: a.agent_status, seq: a.state_change_seq || 0 }))
  } catch (e) {
    console.warn('解析 herdr 输出失败：' + e)
    return null
  }
}

// 每个来源读不到时，过一会儿再试；读到了每轮都读
const sources = [
  { list: listHerdr, retryAt: 0 },
  { list: listSessions, retryAt: 0 },
]

// 两边合起来；都读不到返回 null
function listAgents(now) {
  let out = null
  for (const src of sources) {
    if (now < src.retryAt) continue
    const agents = src.list()
    if (agents === null) {
      src.retryAt = now + MISSING_RETRY_MS
      continue
    }
    out = (out || []).concat(agents)
  }
  return out
}

// label 用在通知正文里：项目 · 等你处理的是什么（gerdr 给的问题、要批准的命令），没有就是任务标题
function label(a) {
  const title = a.detail || a.title || a.id
  return a.project && a.project !== '.' ? `${a.project} · ${title}` : title
}

// ---------- 进入当前状态的时间（手环上算卡了多久） ----------

const since = {} // id → { status, since }；since 为 0 表示 Mac 开始观察之前就是这个状态

function track(agents, first, now) {
  const seen = {}
  for (const a of agents) {
    seen[a.id] = true
    let info = since[a.id]
    if (!info || info.status !== a.status) {
      info = { status: a.status, since: first ? 0 : now }
      since[a.id] = info
    }
    if (info.since) a.since = info.since
  }
  for (const id in since) {
    if (!seen[id]) {
      delete since[id]
      delete reminded[id]
    }
  }
}

// ---------- 每天完成了几个任务 ----------

const WEEKDAYS = ['日', '一', '二', '三', '四', '五', '六']
const pad = (n) => (n < 10 ? '0' : '') + n
const dayKey = (d) => `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`

// { days: { '2026-10-23': 3, ... } }。第一次用时把旧版 App 记的 stats.json 搬过来
function loadStats() {
  const s = storage.get('stats')
  if (s) return s
  const out = { days: {} }
  try {
    const old = JSON.parse(readFile(home + '/Library/Application Support/miband-pet/stats.json'))
    for (const k in old.days || {}) if (old.days[k].done) out.days[k] = old.days[k].done
  } catch (e) {}
  storage.set('stats', out)
  return out
}

const stats = loadStats()

function addDone(n, now) {
  const k = dayKey(new Date(now))
  stats.days[k] = (stats.days[k] || 0) + n
  // 只留最近 400 天
  const keys = Object.keys(stats.days).sort()
  for (const old of keys.slice(0, Math.max(0, keys.length - 400))) delete stats.days[old]
  storage.set('stats', stats)
}

// 推给手环的样子：今天的数 + 最近 7 天（最后一个是今天）
function publishStats(now) {
  const days = []
  for (let i = 6; i >= 0; i--) {
    const d = new Date(now)
    d.setDate(d.getDate() - i)
    days.push({ date: `${pad(d.getMonth() + 1)}-${pad(d.getDate())}`, weekday: WEEKDAYS[d.getDay()], done: stats.days[dayKey(d)] || 0 })
  }
  publish('stats', { today: { done: days[6].done }, days })
}

// ---------- 提醒 ----------

const reminded = {} // id → 已经为哪次卡住（since）再提醒过
let lastWake = 0 // 上次点亮手环的时间（提醒和保持亮屏共用）

const finished = (before, a) => before === 'working' && (a.status === 'done' || a.status === 'idle')

// 跑完（working → done/idle）、卡住（→ blocked）时提醒；打开了「开始跑也提醒」的话 → working 也提醒。
// 新出现的 agent、标题变化不提醒，免得刷屏
function changes(prev, agents) {
  const out = []
  for (const a of agents) {
    const before = prev[a.id]
    if (before === undefined || before === a.status) continue
    const started = config.notifyStart !== false && a.status === 'working'
    if (a.status === 'blocked' || finished(before, a) || started) {
      out.push({ title: `${a.agent || 'agent'} ${STATUS_TEXT[a.status] || a.status}`, body: label(a) })
    }
  }
  return out
}

// 卡住超过设定的分钟数，再提醒一次（每次卡住只提醒一次）
function impatient(agents, now) {
  const limit = impatientMin()
  if (limit <= 0) return []
  const out = []
  for (const a of agents) {
    if (a.status !== 'blocked' || !a.since || reminded[a.id] === a.since) continue
    const mins = Math.floor((now - a.since) / 60000)
    if (mins < limit) continue
    reminded[a.id] = a.since
    out.push({ title: `${a.agent || 'agent'} 等你 ${mins} 分钟了`, body: label(a) })
  }
  return out
}

function wake() {
  wakeBand()
  lastWake = Date.now()
}

function alert(events) {
  const mode = alertMode()
  if (events.length === 0 || mode === 'off') return
  try {
    if (mode === 'wake') {
      wake()
      return
    }
    // 同一轮里多个 agent 一起变化，合成一条，免得连震好几下
    if (events.length === 1) notifyBand(events[0].title, events[0].body)
    else notifyBand(`${events.length} 个 agent 有变化`, events.map((e) => `${e.title}：${e.body}`).join('\n'))
  } catch (e) {
    console.warn('提醒失败：' + e) // 手环没连上，这次的就不补了
  }
}

// 打开了「保持亮屏」且有 agent 等你回答时，隔一会儿把手环点亮一次，手环就不会熄屏
let blocked = false
setInterval(() => {
  const every = Math.min(60, Math.max(5, config.keepAwakeSec || 50)) * 1000
  if (!config.keepAwake || !blocked || Date.now() - lastWake < every) return
  try {
    wake()
  } catch (e) {}
}, 5000)

// ---------- 主循环 ----------

let prev = null // id → 上一轮的状态；null 表示还没读到过

function poll() {
  const now = Date.now()
  const raw = listAgents(now)
  publishStats(now) // 过了零点「今天」也要换
  if (raw === null) {
    prev = null
    blocked = false
    publish('agents', [])
    return setTimeout(poll, POLL_MS)
  }
  const agents = raw.sort((x, y) => (x.id < y.id ? -1 : x.id > y.id ? 1 : 0)) // 顺序固定，内容没变就不会重推
  track(agents, prev === null, now)
  if (prev) {
    const done = agents.filter((a) => prev[a.id] !== undefined && finished(prev[a.id], a)).length
    if (done > 0) {
      addDone(done, now)
      publishStats(now)
    }
  }
  const events = prev ? changes(prev, agents).concat(impatient(agents, now)) : []
  prev = {}
  for (const a of agents) prev[a.id] = a.status
  blocked = agents.some((a) => a.status === 'blocked')
  // 先推列表，过一会儿再提醒：通知先到的话，手环处理通知时后面的消息会堵一秒多，猫换动作就比通知晚好几秒
  publish('agents', agents)
  setTimeout(() => alert(events), 500)
  setTimeout(poll, POLL_MS)
}

poll()
