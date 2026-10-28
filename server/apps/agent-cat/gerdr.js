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
  // id 前面加 gerdr:，免得和 herdr 的窗格 id 撞上；seq 用最后更新的时间（毫秒），同一档里最近动过的在前
  return sessions.map((s) => ({ id: 'gerdr:' + s.id, agent: s.agent, project: project(s.cwd), title: s.title || '', detail: detail(s), status: STATUS[s.status] || 'unknown', seq: s.updatedAt || 0 }))
}

// 等你处理的是什么：问的问题、要批准的命令，或者出错的原因。手环气泡的大字、通知正文用它。
// gerdr 给的问题是「回答：午饭吃什么？（面条 / 米饭）」，去掉「回答：」；要批准的命令留着「审批：」，看得出不是在问问题
function detail(s) {
  let text = ''
  if (s.status === 'waiting') text = (s.waiting || '').replace(/^回答[：:]\s*/, '')
  else if (s.status === 'error' && s.error) text = '出错：' + s.error
  return text.replace(/\s+/g, ' ').trim()
}
