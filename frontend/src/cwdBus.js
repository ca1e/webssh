// ---------- 会话 cwd 变更事件总线 ----------
// 后端 terminalWS 在 OSC 7 钩子命中时推 {"type":"cwd","path":...} 帧,
// TerminalPane 收到后投递到本总线;SFTP 面板等组件按会话订阅驱动目录跟随,
// 免去轮询 GET /cwd。多个终端叶子(分屏)可能重复投递,订阅方按值去重。

const handlers = new Map() // sid -> Set<(path) => void>

// onCwd 订阅指定会话的 cwd 变更,返回取消订阅函数。
export function onCwd(sid, fn) {
  if (!sid || typeof fn !== 'function') return () => {}
  let set = handlers.get(sid)
  if (!set) {
    set = new Set()
    handlers.set(sid, set)
  }
  set.add(fn)
  return () => {
    set.delete(fn)
    if (set.size === 0 && handlers.get(sid) === set) handlers.delete(sid)
  }
}

// emitCwd 由终端 WS 消息处理调用,向该会话的所有订阅者广播新路径。
export function emitCwd(sid, path) {
  const set = handlers.get(sid)
  if (!set) return
  for (const fn of [...set]) {
    try {
      fn(path)
    } catch {
      /* 单个订阅者异常不影响其他订阅者 */
    }
  }
}
