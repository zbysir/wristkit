// 橘黑猫的规则：饱腹、心情两个数值，加上「这会儿在干什么」。
// 手环上的 app 不在后台跑，所以每次打开时从上次存的时间开始，按 5 分钟一步往后推：
// 每一步先定这段时间在干什么，再按它加减数值。都是纯函数，index.ux 调用，也能直接用 node 测。
//
// 干什么，按优先级：
//   1. 点过猫（manualUntil 之前）：照点的那个做
//   2. 太饿就去吃（吃到饱为止），太闷就去玩（玩到开心为止）
//   3. 时间表：夜里睡觉；早中晚饭点吃东西；工作日白天上班，穿插学英语、偶尔摸鱼看电视；
//      其余时间在看电视、玩玩具、学英语里挑，两样都满时很开心。每半小时挑一次，同一时段挑的结果固定
// 显示上再看数值：有点饿（不在吃、不在睡）就显示「饿了」，心情低就显示「不开心」。

export const STEP = 5 * 60 * 1000
const HOUR = 60 * 60 * 1000
const BLOCK = 30 * 60 * 1000
const MAX_CATCH_UP = 48 * HOUR // 太久没打开，只补最近两天
export const MANUAL_MS = 15 * 60 * 1000

// 每小时饱腹、心情变多少
const RATE = {
  sleep: [-3, 0],
  work: [-9, -10],
  english: [-8, -8],
  tv: [-7, 15],
  toy: [-9, 30],
  happy: [-7, 5],
  eat: [80, 0]
}

// 点一下换下一个
export const CYCLE = ['eat', 'toy', 'happy', 'tv', 'english', 'work', 'sleep']

export const MEALS = [7.5, 12, 18.5] // 几点吃饭，每顿半小时

export const DEFAULT_CONFIG = { sleepAt: 23, wakeAt: 7, workStart: 9, workEnd: 18, workdays: true }

export function newState(now) {
  return { food: 80, mood: 80, act: 'tv', last: now, manual: '', manualUntil: 0, day: dayKey(now), log: [] }
}

function clamp(v) {
  return Math.max(0, Math.min(100, v))
}

function dayKey(t) {
  const d = new Date(t)
  return d.getFullYear() * 10000 + (d.getMonth() + 1) * 100 + d.getDate()
}

function hash(n) {
  n = (n ^ 61) ^ (n >>> 16)
  n = n + (n << 3)
  n = n ^ (n >>> 4)
  n = Math.imul(n, 0x27d4eb2d)
  return (n ^ (n >>> 15)) >>> 0
}

function pick(t, choices) {
  // choices: [[动作, 权重]]；同一个半小时挑出来的一样
  let total = 0
  for (const c of choices) total += c[1]
  let r = hash(Math.floor(t / BLOCK)) % total
  for (const c of choices) {
    if (r < c[1]) return c[0]
    r -= c[1]
  }
  return choices[0][0]
}

function inHours(h, from, to) {
  return from <= to ? h >= from && h < to : h >= from || h < to // 跨午夜，比如 23 到 7
}

// 时间表上这会儿该干什么
export function scheduled(t, s, cfg) {
  const d = new Date(t)
  const h = d.getHours() + d.getMinutes() / 60
  if (inHours(h, cfg.sleepAt, cfg.wakeAt)) return 'sleep'
  for (const m of MEALS) if (h >= m && h < m + 0.5) return 'eat'
  const weekday = d.getDay() >= 1 && d.getDay() <= 5
  if (cfg.workdays && weekday && inHours(h, cfg.workStart, cfg.workEnd)) {
    return pick(t, [['work', 6], ['english', 2], ['tv', 1]])
  }
  if (s.food >= 85 && s.mood >= 85) return 'happy'
  return pick(t, [['tv', 2], ['toy', 2], ['english', 1]])
}

// 这一步在干什么
export function decide(t, s, cfg) {
  if (s.manualUntil > t) return s.manual
  if (s.food < 20 || (s.act === 'eat' && s.food < 90 && s.hungry)) return 'eat'
  if (s.mood < 20 || (s.act === 'toy' && s.mood < 80 && s.bored)) return 'toy'
  return scheduled(t, s, cfg)
}

// 看起来在干什么：数值低了显示饿了、不开心
export function look(s) {
  if (s.act !== 'eat' && s.act !== 'sleep' && s.food < 30) return 'hungry'
  if (s.act !== 'toy' && s.act !== 'sleep' && s.mood < 30) return 'sad'
  return s.act
}

// 从 s.last 推到 now，返回新的状态（不改传进来的）
export function advance(s, now, cfg) {
  cfg = Object.assign({}, DEFAULT_CONFIG, cfg || {})
  s = Object.assign({}, s, { log: s.log.slice() })
  let t = Math.max(s.last, now - MAX_CATCH_UP)
  if (now < s.last) t = now // 改过系统时间
  for (;;) {
    const act = decide(t, s, cfg)
    s.hungry = act === 'eat' && (s.food < 20 || s.hungry)
    s.bored = act === 'toy' && (s.mood < 20 || s.bored)
    setAct(s, act, t)
    const dt = Math.min(STEP, now - t)
    if (dt <= 0) break
    const rate = RATE[act]
    s.food = clamp(s.food + (rate[0] * dt) / HOUR)
    s.mood = clamp(s.mood + (rate[1] * dt) / HOUR)
    t += dt
  }
  s.last = now
  return s
}

// 点一下：换下一个，照着做 15 分钟
export function tap(s, now, cfg) {
  s = advance(s, now, cfg)
  const i = CYCLE.indexOf(s.manualUntil > now ? s.manual : s.act)
  s.manual = CYCLE[(i + 1) % CYCLE.length]
  s.manualUntil = now + MANUAL_MS
  s.hungry = false
  s.bored = false
  setAct(s, s.manual, now)
  return s
}

// 换动作，并记到今天的日志里（换了一天就清空）
function setAct(s, act, t) {
  const day = dayKey(t)
  if (day !== s.day) {
    s.day = day
    s.log = []
  }
  const shown = look(Object.assign({}, s, { act }))
  const lastLog = s.log[s.log.length - 1]
  if (!lastLog || lastLog.act !== shown) {
    s.log.push({ t, act: shown })
    if (s.log.length > 30) s.log.shift()
  }
  s.act = act
}
