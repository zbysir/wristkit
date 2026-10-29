// 读 gerdr（Claude Code / Codex 的图形界面）里开着的会话，换成和 herdr 一样的样子，herdr.js 把两边合在一起推给手环。
// 没装 gerdr、gerdr 没开的用户：返回 null，就当没有 gerdr 的会话。
import { exec } from 'miband/server'

// gerdr 的状态 → 猫认得的状态。出错了也要人去看，按「等你处理」算
const STATUS = { running: 'working', waiting: 'blocked', error: 'blocked', idle: 'idle' }

const project = (cwd) => (cwd || '').replace(/\/+$/, '').split('/').pop()

// 返回 agent 列表；没装 gerdr、gerdr 没在跑时返回 null
export function listSessions() {
  let r
  try {
    r = exec('gerdr', ['session', 'list'])
  } catch (e) {
    return null // 没找到命令
  }
  if (r.code !== 0) return null // gerdr 没开
  let sessions
  try {
    sessions = JSON.parse(r.stdout)
  } catch (e) {
    console.warn('解析 gerdr 输出失败：' + e)
    return null
  }
  if (!Array.isArray(sessions)) return null
  const seen = {}
  const out = sessions.map((s) => {
    const id = 'gerdr:' + s.id
    const status = STATUS[s.status] || 'unknown'
    seen[id] = true
    // id 前面加 gerdr:，免得和 herdr 的窗格 id 撞上
    return { id, agent: s.agent, project: project(s.cwd), title: s.title || '', detail: detail(s), status, seq: changedAt(id, status, s.updatedAt) }
  })
  for (const id in changed) if (!seen[id]) delete changed[id]
  return out
}

// seq：状态最后一次变化的时间（毫秒），同一档里刚变过状态的在前。
// 不能直接用 updatedAt：会话在跑时它几秒就变一次，整份 agent 列表（几 KB、要分片）就跟着几秒重推一次给手环，
// 手环一连几个小时处理不过来会自己重启
const changed = {} // id → { status, at }

function changedAt(id, status, updatedAt) {
  const c = changed[id]
  if (!c || c.status !== status) changed[id] = { status, at: updatedAt || Date.now() }
  return changed[id].at
}

// 等你处理的是什么：问的问题、要批准的命令，或者出错的原因。手环气泡的大字、通知正文用它。
// gerdr 给的问题是「回答：午饭吃什么？（面条 / 米饭）」，去掉「回答：」；要批准的命令留着「审批：」，看得出不是在问问题
function detail(s) {
  let text = ''
  if (s.status === 'waiting') text = (s.waiting || '').replace(/^回答[：:]\s*/, '')
  else if (s.status === 'error' && s.error) text = '出错：' + s.error
  return text.replace(/\s+/g, ' ').trim()
}
