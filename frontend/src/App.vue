<script setup>
import { computed, onMounted, reactive, ref, watch } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { ArrowDown, ArrowRight, Box, CircleCloseFilled, Connection, DArrowRight, FolderOpened, Loading, Monitor, Moon, Plus, Promotion, Setting, Sunny } from '@element-plus/icons-vue'
import ConnectionList from './components/ConnectionList.vue'
import HostModal from './components/HostModal.vue'
import SettingsPanel from './components/SettingsPanel.vue'
import TermTree from './components/TermTree.vue'
import SftpPanel from './components/SftpPanel.vue'
import ForwardPanel from './components/ForwardPanel.vue'
import HostPanel from './components/HostPanel.vue'
import DockerPanel from './components/DockerPanel.vue'
import K8sPanel from './components/K8sPanel.vue'
import ContainerLogsPanel from './components/ContainerLogsPanel.vue'
import TerminalPane from './components/TerminalPane.vue'
import LockScreen from './components/LockScreen.vue'
import OverviewPanel from './components/OverviewPanel.vue'
import { authState, authStatus, closeSession, deleteHost, duplicateHost, listHosts, listSessions, openSession } from './api'
import { useContextMenu } from './contextMenu'
import { isDark, settings } from './settings'

const hosts = ref([])
// session: {id, hostId, name(带序号), host, username, jumpName,
//           sftpOpen, fwdOpen, panelPct, tree(终端分割树), focused(聚焦叶子key)}
const sessions = ref([])
const activeId = ref('')
const modalVisible = ref(false)
const editingHost = ref(null)
// 每主机已建立的连接序号(该主机会话全部关闭后复位)
const hostSeq = reactive({})
// 连接中的主机,防止双击/连点造成意外重复会话
const connecting = new Set()
// 进行中的连接请求(tab id → AbortController),关闭 tab 时取消等待
const pendingAborts = new Map()
let pendingSeq = 0
// 连接中 tab 的已等待秒数由单一定时器驱动,无连接中 tab 时自毁
let elapsedTimer = null

function tickElapsed() {
  let any = false
  for (const s of sessions.value) {
    if (s.status === 'connecting') {
      s.elapsed++
      any = true
    }
  }
  if (!any) {
    clearInterval(elapsedTimer)
    elapsedTimer = null
  }
}

function startElapsed(s) {
  s.elapsed = 0
  if (!elapsedTimer) elapsedTimer = setInterval(tickElapsed, 1000)
}

// 主机列表收起状态:收起后仅显示图标宽度的展开条
const sidebarCollapsed = ref(false)
// ConnectionList 事件统一绑定
const listHandlers = {
  onConnect: connect,
  onCollapse: () => (sidebarCollapsed.value = true),
  onEdit: openEdit,
  onDelete: removeHost,
  onDuplicate: duplicate,
  onRefresh: loadHosts,
}

// 终端分割树:叶子 = 终端实例(独立 PTY),split = h(上下)/v(左右)分割节点
let leafSeq = 0
const makeLeaf = () => ({ type: 'leaf', key: ++leafSeq })

function leafCount(node) {
  return node.type === 'leaf' ? 1 : leafCount(node.children[0]) + leafCount(node.children[1])
}

function firstLeafKey(node) {
  return node.type === 'leaf' ? node.key : firstLeafKey(node.children[0])
}

function findNode(node, key) {
  if (node.type === 'leaf') return node.key === key ? node : null
  return findNode(node.children[0], key) || findNode(node.children[1], key)
}

// 就地把叶子节点转换为分割节点(新老叶子各占一半);返回新叶子的 key
function splitNode(node, key, dir) {
  if (node.type === 'leaf') {
    if (node.key !== key) return null
    node.type = 'split'
    node.dir = dir
    const fresh = makeLeaf()
    node.children = [{ type: 'leaf', key }, fresh]
    return fresh.key
  }
  return splitNode(node.children[0], key, dir) || splitNode(node.children[1], key, dir)
}

function findParent(node, key) {
  if (node.type !== 'split') return null
  for (const c of node.children) {
    if (c.type === 'leaf' && c.key === key) return node
    const p = findParent(c, key)
    if (p) return p
  }
  return null
}

// 关闭一个终端叶子:用兄弟子树就地替换其父分割节点
function closeLeaf(s, key) {
  if (leafCount(s.tree) <= 1) return
  const parent = findParent(s.tree, key)
  if (!parent) return
  const [a, b] = parent.children
  const keyOf = (n) => (n.type === 'leaf' ? n.key : null)
  const dying = keyOf(a) === key ? a : keyOf(b) === key ? b : null
  if (!dying) return
  const keep = dying === a ? b : a
  const copy = JSON.parse(JSON.stringify(keep))
  delete parent.key
  delete parent.type
  delete parent.dir
  delete parent.children
  Object.assign(parent, copy)
  if (s.focused === key) s.focused = firstLeafKey(s.tree)
}

function splitTerm(s, dir) {
  const key = s.focused && findNode(s.tree, s.focused) ? s.focused : firstLeafKey(s.tree)
  const fresh = splitNode(s.tree, key, dir)
  if (fresh) s.focused = fresh
}

const activeSession = computed(() => sessions.value.find((s) => s.id === activeId.value))

// ---------- 容器/Pod 日志 / Shell 标签页(从 Docker 或 K8s 面板打开) ----------
// type: 'container-logs'|'container-shell' 按 cid 去重;
//       'pod-logs'|'pod-shell' 按 ns/pod(/container) 去重,附 ns/pod/container 字段。
const containerTabs = ref([]) // {id, type, parentSid, cid, ns, pod, container, name, host, label}
let containerSeq = 0

// tab 去重键:容器用容器 ID,Pod 用 ns/pod(/container)
function containerTabKey(t) {
  return t.type && t.type.startsWith('pod-')
    ? `${t.ns}/${t.pod}:${t.container || ''}`
    : t.cid
}

function openContainerTab(payload) {
  const { type, sid, name } = payload
  // 找到父会话,取主机 IP
  const parentSession = sessions.value.find((s) => s.id === sid)
  const hostIP = parentSession?.host || ''
  // 同一目标同类型不重复开
  const key = containerTabKey(payload)
  const existing = containerTabs.value.find((t) => containerTabKey(t) === key && t.type === type && t.parentSid === sid)
  if (existing) {
    activeId.value = existing.id
    return
  }
  const id = `ctab-${++containerSeq}`
  containerTabs.value.push({
    id,
    type,
    parentSid: sid,
    cid: payload.cid || '',
    ns: payload.ns || '',
    pod: payload.pod || '',
    container: payload.container || '',
    name,
    host: hostIP,
    label: type === 'container-logs' || type === 'pod-logs' ? `${name} 📋` : `${name} 🖥`,
  })
  activeId.value = id
}

function closeContainerTab(id) {
  const idx = containerTabs.value.findIndex((t) => t.id === id)
  if (idx < 0) return
  containerTabs.value.splice(idx, 1)
  const allIds = [
    ...sessions.value.map((s) => s.id),
    ...containerTabs.value.map((t) => t.id),
  ]
  if (activeId.value === id) {
    activeId.value = allIds.length ? allIds[Math.min(idx, allIds.length - 1)] : ''
  }
}

// ---------- 刷新恢复:Tab 布局持久化(sessionStorage,按浏览器标签页隔离) ----------
// F5 后 Vue 内存清空,但后端 Session 由 keepalive 独立存活;恢复 = 读本标签页的
// 布局记录,与 GET /api/sessions 的活会话求交集重建 Tab。旧 PTY 已随 WS 断开被
// 远端回收,恢复后的终端是新 shell(与手动点"重新连接"语义一致)。
const TABS_KEY = 'webssh.tabs'
let restoring = false
let restoredOnce = false

// 终端树与叶子 key 无关的纯形状(leafSeq 每次加载重新分配,聚焦位置存索引路径)
function treeShape(node) {
  return node.type === 'leaf' ? { t: 'l' } : { t: 's', d: node.dir, c: [treeShape(node.children[0]), treeShape(node.children[1])] }
}

function rebuildTree(shape) {
  if (shape.t === 'l') return makeLeaf()
  return { type: 'split', dir: shape.d, children: [rebuildTree(shape.c[0]), rebuildTree(shape.c[1])] }
}

function leafPath(node, key, path = []) {
  if (node.type === 'leaf') return node.key === key ? path : null
  for (let i = 0; i < 2; i++) {
    const p = leafPath(node.children[i], key, [...path, i])
    if (p) return p
  }
  return null
}

function keyAtLeafPath(node, path) {
  let n = node
  for (const i of path) {
    if (!n || n.type !== 'split') return null
    n = n.children[i]
  }
  return n && n.type === 'leaf' ? n.key : null
}

function collectTabsRecord() {
  return {
    v: 1,
    sessions: sessions.value
      .filter((s) => s.status === 'open')
      .map((s) => ({
        id: s.id,
        hostId: s.hostId,
        name: s.name,
        host: s.host,
        username: s.username,
        connType: s.connType,
        jumpName: s.jumpName,
        sftpOpen: s.sftpOpen,
        fwdOpen: s.fwdOpen,
        hostOpen: s.hostOpen,
        dockerOpen: s.dockerOpen,
        k8sOpen: s.k8sOpen,
        panelPct: s.panelPct,
        shape: treeShape(s.tree),
        focus: leafPath(s.tree, s.focused) || [],
      })),
    containerTabs: containerTabs.value.map((t) => ({ ...t })),
    activeId: activeId.value,
    hostSeq: { ...hostSeq },
  }
}

let persistTimer = null
function schedulePersist() {
  if (restoring) return
  clearTimeout(persistTimer)
  persistTimer = setTimeout(() => {
    try {
      sessionStorage.setItem(TABS_KEY, JSON.stringify(collectTabsRecord()))
    } catch {
      /* 隐私模式/配额受限:放弃持久化,不影响正常使用 */
    }
  }, 300)
}
watch([sessions, containerTabs, activeId], schedulePersist, { deep: true })

async function restoreTabs() {
  if (restoredOnce || restoring) return
  let rec
  try {
    rec = JSON.parse(sessionStorage.getItem(TABS_KEY) || 'null')
  } catch {
    rec = null
  }
  if (!rec || !Array.isArray(rec.sessions) || !rec.sessions.length) {
    restoredOnce = true
    return
  }
  restoring = true
  try {
    const live = new Map((await listSessions()).map((x) => [x.id, x]))
    for (const r of rec.sessions) {
      const lv = live.get(r.id)
      let tree
      try {
        tree = rebuildTree(r.shape)
      } catch {
        tree = makeLeaf()
      }
      // 后端已无此会话(keepalive 检出断链/服务重启):保留 tab 置为 failed,
      // 凭记录中的 hostId 走既有"重试"流程重连
      sessions.value.push(
        reactive({
          id: r.id,
          hostId: r.hostId,
          status: lv ? 'open' : 'failed',
          errMsg: lv ? '' : '会话已失效(服务端连接断开或已重启),点击重试重新连接',
          name: r.name,
          host: (lv && lv.host) || r.host,
          username: (lv && lv.username) || r.username,
          connType: (lv && lv.conn_type) || r.connType,
          jumpName: lv ? lv.jump_name || '' : r.jumpName || '',
          flash: false,
          sftpOpen: !!r.sftpOpen,
          fwdOpen: !!r.fwdOpen,
          hostOpen: !!r.hostOpen,
          dockerOpen: !!r.dockerOpen,
          k8sOpen: !!r.k8sOpen,
          panelPct: r.panelPct || 35,
          tree,
          focused: keyAtLeafPath(tree, r.focus || []) || firstLeafKey(tree),
          elapsed: 0,
        }),
      )
    }
    Object.assign(hostSeq, rec.hostSeq || {})
    const openIds = new Set(sessions.value.filter((s) => s.status === 'open').map((s) => s.id))
    for (const t of Array.isArray(rec.containerTabs) ? rec.containerTabs : []) {
      if (!openIds.has(t.parentSid)) continue
      containerTabs.value.push(t)
      const n = Number(String(t.id).replace('ctab-', ''))
      if (n > containerSeq) containerSeq = n
    }
    const allIds = [...sessions.value.map((s) => s.id), ...containerTabs.value.map((t) => t.id)]
    activeId.value = allIds.includes(rec.activeId) ? rec.activeId : allIds[0] || ''
  } catch {
    return // listSessions 失败(锁屏竞态/网络):本次不恢复,保留记录等解锁后重试
  } finally {
    restoring = false
  }
  restoredOnce = true
  schedulePersist() // 立即回写,清理已失效的会话 id
}

function onTabRemove(id) {
  if (id === 'settings-tab' || id.startsWith('ctab-')) closeContainerTab(id)
  else removeSession(id)
}

// ---------- Tab 右键菜单:关闭 当前/其他/左侧/右侧/全部 ----------
// 弹出朝向随视口剩余空间自动翻转
const tabMenu = reactive({ show: false, x: 0, y: 0, targetId: '' })
const tabMenuEl = ref(null)
const { open: openTabMenuAt, close: closeTabMenu } = useContextMenu(tabMenu)

// tab 的显示顺序 = 会话 tabs 在前、容器 tabs(设置/容器日志/容器 shell)在后
function orderedTabIds() {
  return [
    ...sessions.value.map((s) => s.id),
    ...containerTabs.value.map((t) => t.id),
  ]
}

// 右键目标 tab 两侧的可关闭数量,驱动菜单项禁用态
const tabMenuCtx = computed(() => {
  const ids = orderedTabIds()
  const idx = ids.indexOf(tabMenu.targetId)
  return { total: ids.length, left: idx, right: ids.length - 1 - idx }
})

function openTabMenu(id, x, y) {
  tabMenu.targetId = id
  openTabMenuAt(x, y, tabMenuEl)
}

// 事件委托:右键落在任意 tab 项上(含空白处/关闭按钮附近)都视为对该 tab 的操作;
// 未命中(空白 tab 条区域)不拦截浏览器原生菜单
function onTabsContextmenu(e) {
  const item = e.target.closest?.('.el-tabs__item')
  if (!item) {
    closeTabMenu() // 右键空白处:收起已打开的菜单,原生菜单照常
    return
  }
  const items = e.currentTarget.querySelectorAll('.el-tabs__header .el-tabs__item')
  const idx = [...items].indexOf(item)
  const ids = orderedTabIds()
  if (idx < 0 || idx >= ids.length) return
  e.preventDefault()
  openTabMenu(ids[idx], e.clientX, e.clientY)
}

function removeTabById(id) {
  if (id === 'settings-tab' || id.startsWith('ctab-')) closeContainerTab(id)
  else removeSession(id)
}

// 批量关闭:焦点所在 tab 存活则保持,否则落到幸存的操作目标上
function closeTabsBatch(ids, survivor) {
  if (!ids.length) return
  ids.forEach(removeTabById)
  const remaining = orderedTabIds()
  if (remaining.includes(activeId.value)) return
  if (survivor && remaining.includes(survivor)) activeId.value = survivor
}

function onTabMenuAction(action) {
  const ids = orderedTabIds()
  const idx = ids.indexOf(tabMenu.targetId)
  closeTabMenu()
  if (idx < 0) return // 目标 tab 已不存在(如连接中转为正式会话)
  switch (action) {
    case 'self':
      removeTabById(tabMenu.targetId)
      break
    case 'others':
      closeTabsBatch(ids.filter((id) => id !== tabMenu.targetId), tabMenu.targetId)
      break
    case 'left':
      closeTabsBatch(ids.slice(0, idx), tabMenu.targetId)
      break
    case 'right':
      closeTabsBatch(ids.slice(idx + 1), tabMenu.targetId)
      break
    case 'all':
      closeTabsBatch(ids)
      break
  }
}

// ---------- Tab 栏滚轮:切换激活标签 ----------
let tabsWheelAccum = 0
let tabsWheelReset = null

function onTabsWheel(e) {
  // 仅 tab 栏头部生效:终端等面板区域的滚轮不拦截,scrollback 滚动不受影响
  if (!e.target.closest?.('.el-tabs__header')) return
  if (e.shiftKey) return // Shift+滚轮保留给 Element Plus 内置的标签条平移
  const ids = orderedTabIds()
  if (ids.length <= 1) return
  e.preventDefault()
  // 累计阈值:鼠标一格(deltaY≈100)切一个,触控板小幅滚动可累积;空闲后重置
  tabsWheelAccum += e.deltaY
  clearTimeout(tabsWheelReset)
  tabsWheelReset = setTimeout(() => (tabsWheelAccum = 0), 180)
  if (Math.abs(tabsWheelAccum) < 50) return
  const step = tabsWheelAccum > 0 ? 1 : -1
  tabsWheelAccum = 0
  const idx = ids.indexOf(activeId.value)
  const next = Math.min(Math.max(idx + step, 0), ids.length - 1)
  if (next !== idx) activeId.value = ids[next] // EP 自动把激活 tab 滚入可视区
}

// ---------- 顶栏按钮 ----------
// 主题按钮:与设置页同一配置,单击循环 浅色 → 深色 → 跟随系统
const themeTitle = computed(
  () => ({ light: '主题:浅色', dark: '主题:深色', system: '主题:跟随系统' }[settings.theme])
)
const themeIcon = computed(() => (isDark.value ? Moon : Sunny))
function toggleTheme() {
  settings.theme = settings.theme === 'light' ? 'dark' : settings.theme === 'dark' ? 'system' : 'light'
}
function openGithub() {
  window.open('https://github.com/ca1e/webssh', '_blank')
}

// ---------- 设置 tab ----------
function openSettings() {
  const id = 'settings-tab'
  const existing = sessions.value.find((s) => s.id === id) ||
    containerTabs.value.find((t) => t.id === id)
  if (existing) {
    activeId.value = id
    return
  }
  containerTabs.value.push({
    id,
    type: 'settings',
    name: '设置',
    label: '⚙ 设置',
  })
  activeId.value = id
}

async function loadHosts() {
  try {
    hosts.value = await listHosts()
  } catch (e) {
    ElMessage.error(e.message)
  }
}
// 启动密码锁屏期间不加载业务数据;解锁/任意请求 401 时经 authState 切换渲染
async function checkLock() {
  try {
    const st = await authStatus()
    authState.hasPassword = !!st.has_password
    authState.locked = !!st.locked
  } catch {
    /* 后端不可达:保持默认不锁,由具体请求的错误提示兜底 */
  }
}

onMounted(async () => {
  await checkLock()
  if (!authState.locked) {
    await loadHosts()
    restoreTabs()
  }
})

function onUnlocked() {
  loadHosts()
  restoreTabs()
}

async function connect(host, trustNewKey = false) {
  if (connecting.has(host.id)) {
    // 同一主机已在连接中:保留多开语义(连接完成后可再次点击新开),
    // 但明确反馈而非静默吞掉点击
    flashPending(host.id)
    return
  }
  const n = (hostSeq[host.id] || 0) + 1
  hostSeq[host.id] = n
  const tree = makeLeaf()
  // 乐观 tab:点击即建 tab 展示连接状态,拨号成功后原地升级为终端。
  // 必须用 reactive 包裹:runConnect 在 push 之后仍会改写 s 的字段,
  // 原始对象上的赋值不经过代理、不触发重渲染(tab 会永久停在连接中)。
  const s = reactive({
    id: `pending-${++pendingSeq}`,
    hostId: host.id,
    status: 'connecting', // connecting | open | failed
    errMsg: '',
    name: n === 1 ? host.name : `${host.name}(${n})`,
    host: host.host,
    username: host.username,
    connType: host.conn_type === 'telnet' ? 'telnet' : 'ssh',
    jumpName: hosts.value.find((x) => x.id === host.jump_id)?.name || '',
    flash: false,
    sftpOpen: false,
    fwdOpen: false,
    hostOpen: false,
    dockerOpen: false,
    k8sOpen: false,
    panelPct: 35, // 右侧功能面板宽度百分比(30-70 之间可拖动)
    tree,
    focused: tree.key,
  })
  sessions.value.push(s)
  activeId.value = s.id
  await runConnect(s, host, trustNewKey)
}

// runConnect 执行一次建立会话的请求并驱动 tab 状态机;失败后 tab 停留在
// failed 态(完整错误信息 + 重试),不再使用转瞬即逝的 toast。
async function runConnect(s, host, trustNewKey) {
  if (connecting.has(host.id)) return
  connecting.add(host.id)
  const tabId = s.id
  const ac = new AbortController()
  pendingAborts.set(tabId, ac)
  startElapsed(s)
  // trustRetry 标记:命中"密钥变更"且用户确认信任后,需要在释放锁之后重试。
  // 否则递归调用会被 connecting.has 守卫拦截(锁尚未释放),导致点击信任无反应。
  let trustRetry = false
  try {
    const res = await openSession(host.id, trustNewKey, ac.signal)
    if (!sessions.value.includes(s)) {
      // tab 已在连接完成前被关闭:立即回收刚建立的后端会话
      closeSession(res.session_id).catch(() => {})
      return
    }
    const wasActive = activeId.value === tabId
    s.id = res.session_id
    s.host = res.host
    s.username = res.username
    if (res.conn_type) s.connType = res.conn_type
    s.jumpName = res.jump_name || ''
    s.status = 'open'
    if (wasActive) activeId.value = s.id
  } catch (e) {
    if (ac.signal.aborted || !sessions.value.includes(s)) return // 用户已取消/关闭 tab
    s.status = 'failed'
    s.errMsg = e.message
    if (e.message.includes('信任新密钥')) {
      try {
        await ElMessageBox.confirm(`${e.message}`, '主机密钥变更', {
          type: 'warning',
          confirmButtonText: '信任新密钥并连接',
          cancelButtonText: '取消',
        })
        trustRetry = true
      } catch {
        // 停留在失败页,可手动重试或关闭
      }
    }
  } finally {
    connecting.delete(host.id)
    pendingAborts.delete(tabId)
  }
  if (trustRetry) {
    s.status = 'connecting'
    s.errMsg = ''
    await runConnect(s, host, true)
  }
}

// retryConnect 在失败的 tab 内重试(复用 tab,不新增)
function retryConnect(s) {
  if (connecting.has(s.hostId)) return
  const host = hosts.value.find((h) => h.id === s.hostId) ||
    { id: s.hostId, name: s.name, host: s.host, username: s.username, conn_type: s.connType }
  s.status = 'connecting'
  s.errMsg = ''
  runConnect(s, host, false)
}

// flashPending 闪动正在连接中的 tab 标题,反馈被守卫拦截的重复点击
function flashPending(hostId) {
  const p = sessions.value.find((x) => x.hostId === hostId && x.status === 'connecting')
  if (!p) return
  p.flash = false
  setTimeout(() => {
    p.flash = true
    setTimeout(() => (p.flash = false), 900)
  }, 30)
}

async function removeSession(id) {
  const idx = sessions.value.findIndex((s) => s.id === id)
  if (idx < 0) return
  const [gone] = sessions.value.splice(idx, 1)
  // 连接中的 tab:取消等待(后端会话尚未建立,无需 closeSession);
  // 若请求此刻恰好已完成,runConnect 的竞态分支会回收刚建立的会话
  const ac = pendingAborts.get(id)
  if (ac) ac.abort()
  if (sessions.value.length && activeId.value === id) {
    activeId.value = sessions.value[Math.max(0, idx - 1)].id
  }
  if (!sessions.value.some((s) => s.hostId === gone.hostId)) {
    delete hostSeq[gone.hostId]
  }
  if (gone.status === 'open') {
    // 关闭 Tab 即断开会话
    closeSession(gone.id).catch(() => {})
  }
}

function togglePanel(session, which) {
  const flags = ['sftpOpen', 'fwdOpen', 'hostOpen', 'dockerOpen', 'k8sOpen']
  const key = which + 'Open'
  // 打开当前则关闭其他
  if (!session[key]) {
    flags.forEach((f) => { session[f] = false })
  }
  session[key] = !session[key]
}

// 拖动分隔条调整终端与功能面板的宽度比例(限制 30%-70%,任一边不可被完全隐藏)
function startResize(session, e) {
  e.preventDefault()
  const content = e.target.parentElement
  const rect = content.getBoundingClientRect()
  const onMove = (ev) => {
    const pct = ((rect.right - ev.clientX) / rect.width) * 100
    session.panelPct = Math.min(70, Math.max(30, pct))
  }
  const onUp = () => {
    window.removeEventListener('mousemove', onMove)
    window.removeEventListener('mouseup', onUp)
    document.body.style.cursor = ''
    document.body.style.userSelect = ''
  }
  window.addEventListener('mousemove', onMove)
  window.addEventListener('mouseup', onUp)
  document.body.style.cursor = 'col-resize'
  document.body.style.userSelect = 'none'
}

function openCreate() {
  editingHost.value = null
  modalVisible.value = true
}

function openEdit(host) {
  editingHost.value = host
  modalVisible.value = true
}

async function removeHost(host) {
  try {
    await ElMessageBox.confirm(
      `确认删除连接「${host.name}」(${host.host})?保存的密码将从配置中移除。`,
      '删除确认',
      { type: 'warning' }
    )
  } catch {
    return
  }
  try {
    await deleteHost(host.id)
    await loadHosts()
  } catch (e) {
    ElMessage.error(e.message)
  }
}

async function duplicate(host) {
  try {
    await duplicateHost(host.id)
    await loadHosts()
    ElMessage.success(`已复制为「${host.name} - copy」`)
  } catch (e) {
    ElMessage.error(e.message)
  }
}
</script>

<template>
  <div class="layout">
    <LockScreen v-if="authState.locked" @unlocked="onUnlocked" />
    <header class="topbar">
      <el-button
        type="primary"
        :icon="Plus"
        @click="openCreate"
      >
        新建连接
      </el-button>
      <span class="spacer" />
      <div class="topbar-actions">
        <el-button :icon="themeIcon" circle :title="themeTitle" @click="toggleTheme" />
        <el-button circle title="GitHub" @click="openGithub">
          <svg class="github-icon" viewBox="0 0 16 16" width="16" height="16" fill="currentColor" aria-hidden="true">
            <path
              d="M8 0C3.58 0 0 3.58 0 8c0 3.54 2.29 6.53 5.47 7.59.4.07.55-.17.55-.38 0-.19-.01-.82-.01-1.49-2.01.37-2.53-.49-2.69-.94-.09-.23-.48-.94-.82-1.13-.28-.15-.68-.52-.01-.53.63-.01 1.08.58 1.23.82.72 1.21 1.87.87 2.33.66.07-.52.28-.87.51-1.07-1.78-.2-3.64-.89-3.64-3.95 0-.87.31-1.59.82-2.15-.08-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82.64-.18 1.32-.27 2-.27s1.36.09 2 .27c1.53-1.04 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82 1.27.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48 0 1.07-.01 1.93-.01 2.2 0 .21.15.46.55.38A8.01 8.01 0 0 0 16 8c0-4.42-3.58-8-8-8Z"
            />
          </svg>
        </el-button>
        <el-button :icon="Setting" circle title="设置" @click="openSettings" />
      </div>
    </header>

    <div class="main">
      <ConnectionList v-if="!sidebarCollapsed" :hosts="hosts" v-bind="listHandlers" />
      <div v-else class="sidebar-collapsed">
        <el-button text circle :icon="DArrowRight" title="展开主机列表" @click="sidebarCollapsed = false" />
      </div>

      <div class="session-area">
        <OverviewPanel v-if="!sessions.length && !containerTabs.length" :hosts="hosts" @create="openCreate" />
        <el-tabs
          v-else
          v-model="activeId"
          type="card"
          closable
          class="session-tabs"
          @tab-remove="onTabRemove"
          @contextmenu="onTabsContextmenu"
          @wheel="onTabsWheel"
        >
          <el-tab-pane v-for="s in sessions" :key="s.id" :name="s.id">
            <template #label>
              <span class="tab-label" :class="{ 'tab-flash': s.flash }">
                <el-icon v-if="s.status === 'connecting'" class="is-loading tab-status-icon"><Loading /></el-icon>
                <el-icon v-else-if="s.status === 'failed'" class="tab-status-icon tab-failed"><CircleCloseFilled /></el-icon>
                {{ s.name }}
              </span>
            </template>
            <!-- 连接中:乐观 tab 状态页,拨号完成后原地切换为终端 -->
            <div v-if="s.status === 'connecting'" class="conn-state">
              <el-icon :size="30" class="is-loading conn-spin"><Loading /></el-icon>
              <div class="conn-title">正在连接 {{ s.connType === 'telnet' ? s.host : `${s.username}@${s.host}` }}</div>
              <div v-if="s.jumpName" class="conn-sub">经跳板机 {{ s.jumpName }}</div>
              <div class="conn-elapsed">
                已等待 {{ s.elapsed || 0 }} 秒<span v-if="(s.elapsed || 0) >= 5">,仍在等待远端响应…</span>
              </div>
              <el-button size="small" @click="removeSession(s.id)">取消</el-button>
            </div>
            <!-- 连接失败:完整错误信息落页 + 重试 -->
            <div v-else-if="s.status === 'failed'" class="conn-state">
              <el-icon :size="34" class="conn-fail-icon"><CircleCloseFilled /></el-icon>
              <div class="conn-title">连接失败</div>
              <div class="conn-err">{{ s.errMsg }}</div>
              <div class="conn-actions">
                <el-button type="primary" size="small" @click="retryConnect(s)">重试</el-button>
                <el-button size="small" @click="removeSession(s.id)">关闭</el-button>
              </div>
            </div>
            <template v-else>
            <div class="session-toolbar">
              <template v-if="s.connType !== 'telnet'">
              <el-button
                size="small"
                :type="s.sftpOpen ? 'primary' : 'default'"
                plain
                :icon="FolderOpened"
                @click="togglePanel(s, 'sftp')"
              >
                SFTP
              </el-button>
              <el-button
                size="small"
                :type="s.fwdOpen ? 'primary' : 'default'"
                plain
                :icon="Promotion"
                @click="togglePanel(s, 'fwd')"
              >
                代理 / 转发
              </el-button>
              <el-button
                size="small"
                :type="s.hostOpen ? 'primary' : 'default'"
                plain
                :icon="Monitor"
                title="主机详情:架构/CPU/内存/磁盘/进程"
                @click="togglePanel(s, 'host')"
              >
                主机详情
              </el-button>
              <el-button
                size="small"
                :type="s.dockerOpen ? 'primary' : 'default'"
                plain
                :icon="Box"
                title="Docker 管理:容器列表/inspect"
                @click="togglePanel(s, 'docker')"
              >
                Docker
              </el-button>
              <el-button
                size="small"
                :type="s.k8sOpen ? 'primary' : 'default'"
                plain
                :icon="Connection"
                title="K8s 管理:集群状态/Pod 列表/日志/终端(远端需 kubectl)"
                @click="togglePanel(s, 'k8s')"
              >
                K8s
              </el-button>
              </template>
              <el-divider direction="vertical" />
              <el-button
                size="small"
                plain
                :icon="ArrowRight"
                title="左右分屏:当前终端拆分为两个终端"
                @click="splitTerm(s, 'v')"
              >
                左右分屏
              </el-button>
              <el-button
                size="small"
                plain
                :icon="ArrowDown"
                title="上下分屏:当前终端拆分为两个终端"
                @click="splitTerm(s, 'h')"
              >
                上下分屏
              </el-button>
              <span class="muted toolbar-info">
                {{ s.connType === 'telnet' ? s.host : `${s.username}@${s.host}` }}
                <template v-if="s.jumpName">(经跳板机 {{ s.jumpName }})</template>
              </span>
            </div>
            <div class="session-content">
              <TermTree
                :tree="s.tree"
                :sid="s.id"
                :host="s.host"
                :active="s.id === activeId"
                :count="leafCount(s.tree)"
                @close="(k) => closeLeaf(s, k)"
                @focus="(k) => (s.focused = k)"
              />
              <div
                class="splitter"
                v-if="s.sftpOpen || s.fwdOpen || s.hostOpen || s.dockerOpen || s.k8sOpen"
                title="拖动调整宽度(30%-70%)"
                @mousedown="startResize(s, $event)"
              ></div>
              <div
                class="side-panel"
                v-if="s.sftpOpen || s.fwdOpen || s.hostOpen || s.dockerOpen || s.k8sOpen"
                :style="{ width: (s.panelPct || 35) + '%' }"
              >
                <SftpPanel v-if="s.sftpOpen" :session="s" :sessions="sessions" />
                <ForwardPanel v-else-if="s.fwdOpen" :session="s" />
                <HostPanel v-else-if="s.hostOpen" :session="s" />
                <DockerPanel v-else-if="s.dockerOpen" :session="s" @open-tab="openContainerTab" />
                <K8sPanel v-else-if="s.k8sOpen" :session="s" @open-tab="openContainerTab" />
              </div>
            </div>
            </template>
          </el-tab-pane>
          <el-tab-pane v-for="ct in containerTabs" :key="ct.id" :name="ct.id">
            <template #label>
              <span class="tab-label">{{ ct.label }}</span>
            </template>
            <div v-if="ct.type === 'settings'" class="session-content">
              <SettingsPanel @close="closeContainerTab('settings-tab')" />
            </div>
            <div v-else-if="ct.type === 'container-logs'" class="session-content" style="background: #0d1117">
              <ContainerLogsPanel
                :ws-url="`/api/sessions/${ct.parentSid}/docker/logs/ws?cid=${ct.cid}&tail=200`"
                :name="ct.name"
                :host="ct.host"
                style="flex:1; display:flex; flex-direction:column"
              />
            </div>
            <div v-else-if="ct.type === 'pod-logs'" class="session-content" style="background: #0d1117">
              <ContainerLogsPanel
                :ws-url="`/api/sessions/${ct.parentSid}/k8s/logs/ws?namespace=${encodeURIComponent(ct.ns)}&pod=${encodeURIComponent(ct.pod)}${ct.container ? `&container=${encodeURIComponent(ct.container)}` : ''}&tail=200`"
                :name="ct.pod"
                :host="ct.host"
                style="flex:1; display:flex; flex-direction:column"
              />
            </div>
            <div v-else-if="ct.type === 'pod-shell'" class="session-content" style="background: #0d1117; flex-direction: column">
              <div class="container-shell-header">
                <span>Pod 终端 · {{ ct.pod }} <span class="muted">({{ ct.ns }} · {{ ct.host }})</span></span>
              </div>
              <TerminalPane
                :sid="ct.parentSid"
                :host="ct.pod"
                :active="ct.id === activeId"
                :ws-url="`/api/sessions/${ct.parentSid}/k8s/exec/ws?namespace=${encodeURIComponent(ct.ns)}&pod=${encodeURIComponent(ct.pod)}${ct.container ? `&container=${encodeURIComponent(ct.container)}` : ''}&cols=120&rows=30`"
                style="flex:1"
              />
            </div>
            <div v-else class="session-content" style="background: #0d1117; flex-direction: column">
              <div class="container-shell-header">
                <span>容器终端 · {{ ct.name }} <span class="muted">({{ ct.host }})</span></span>
              </div>
              <TerminalPane
                :sid="ct.parentSid"
                :host="ct.name"
                :active="ct.id === activeId"
                :ws-url="`/api/sessions/${ct.parentSid}/docker/shell/ws?cid=${ct.cid}&cols=120&rows=30`"
                style="flex:1"
              />
            </div>
          </el-tab-pane>
        </el-tabs>
      </div>
    </div>

    <!-- Tab 右键菜单 -->
    <teleport to="body">
      <div
        v-if="tabMenu.show"
        ref="tabMenuEl"
        class="ctx-menu"
        :style="{ left: tabMenu.x + 'px', top: tabMenu.y + 'px' }"
        @contextmenu.prevent
      >
        <div class="ctx-item" @click.stop="onTabMenuAction('self')">关闭当前标签</div>
        <div
          class="ctx-item"
          :class="{ disabled: tabMenuCtx.total <= 1 }"
          @click.stop="tabMenuCtx.total > 1 && onTabMenuAction('others')"
        >
          关闭其他标签
        </div>
        <div
          class="ctx-item"
          :class="{ disabled: tabMenuCtx.left <= 0 }"
          @click.stop="tabMenuCtx.left > 0 && onTabMenuAction('left')"
        >
          关闭左侧标签
        </div>
        <div
          class="ctx-item"
          :class="{ disabled: tabMenuCtx.right <= 0 }"
          @click.stop="tabMenuCtx.right > 0 && onTabMenuAction('right')"
        >
          关闭右侧标签
        </div>
        <div class="ctx-item danger" @click.stop="onTabMenuAction('all')">关闭全部标签</div>
      </div>
    </teleport>

    <HostModal v-model="modalVisible" :host="editingHost" :hosts="hosts" @saved="loadHosts" />
  </div>
</template>
