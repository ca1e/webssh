<script setup>
import { reactive, ref, onMounted } from 'vue'
import { ElMessage } from 'element-plus'
import { settings } from '../settings'
import { authChangePassword, listHosts, mcpGetSettings, mcpSaveSettings } from '../api'

defineEmits(['close'])

// 左侧导航项;右侧面板按 activeMenu 渲染
const menus = [
  { key: 'general', label: '通用' },
  { key: 'security', label: '安全' },
  { key: 'mcp', label: 'MCP' },
  { key: 'about', label: '关于' },
]
const activeMenu = ref('general')

// ---------- 安全:修改启动密码 ----------
const pwdForm = reactive({ current: '', next: '', repeat: '' })
const pwdSaving = ref(false)

async function submitChangePassword() {
  if (!pwdForm.current) return ElMessage.warning('请输入当前密码')
  if (pwdForm.next.length < 4) return ElMessage.warning('新密码至少 4 位')
  if (pwdForm.next !== pwdForm.repeat) return ElMessage.warning('两次输入的新密码不一致')
  if (pwdForm.next === pwdForm.current) return ElMessage.warning('新密码不能与当前密码相同')
  pwdSaving.value = true
  try {
    await authChangePassword(pwdForm.current, pwdForm.next)
    ElMessage.success('密码已修改')
    pwdForm.current = pwdForm.next = pwdForm.repeat = ''
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    pwdSaving.value = false
  }
}

// ---------- 通用:主题 ----------
const themeOptions = [
  { value: 'light', title: '浅色', desc: '默认浅色配色。' },
  { value: 'dark', title: '深色', desc: '深色配色,适合低光环境。' },
  { value: 'system', title: '跟随系统', desc: '自动跟随系统深浅色,不支持时保持浅色。' },
]

// ---------- 通用:侧边栏打开方式 ----------
const openModeOptions = [
  { value: 'click', title: '单击打开', desc: '单击主机即可打开连接。' },
  { value: 'dblclick', title: '双击打开', desc: '双击主机打开连接。' },
]

// ---------- MCP:路径白名单 / 可见主机 / 工具权限 ----------
const wl = reactive({ enabled: false, paths: [] })
const vh = reactive({ enabled: false, ids: [] })
const tf = ref([]) // 工具白名单:显式列表,空 = 不暴露任何工具
const mcpLoaded = ref(false)
const newPath = ref('')
const mcpSaving = ref(false)
// 全部主机(按分组组织)与工具目录
const hostGroups = ref([])
const toolCatalog = [
  { name: 'ssh_list_hosts', label: '主机列表' },
  { name: 'ssh_host_info', label: '主机详情' },
  { name: 'ssh_test_connection', label: '测试连接' },
  { name: 'ssh_exec', label: '执行命令' },
  { name: 'ssh_download_file', label: '文件下载' },
  { name: 'ssh_system_info', label: '系统信息' },
  { name: 'ssh_cpu_memory', label: 'CPU/内存' },
  { name: 'ssh_disk_usage', label: '磁盘空间' },
  { name: 'ssh_network_status', label: '网络状态' },
  { name: 'ssh_service_status', label: '服务状态' },
  { name: 'ssh_docker_status', label: 'Docker 状态' },
  { name: 'ssh_k8s_status', label: 'K8s 状态' },
]

async function saveMcpSettings() {
  mcpSaving.value = true
  try {
    const res = await mcpSaveSettings({
      path_whitelist: { enabled: wl.enabled, paths: wl.paths },
      visible_hosts: { enabled: vh.enabled, ids: vh.ids },
      tool_filter: { tools: tf.value },
    })
    wl.enabled = res.path_whitelist.enabled
    wl.paths = res.path_whitelist.paths || []
    vh.enabled = res.visible_hosts.enabled
    vh.ids = res.visible_hosts.ids || []
    tf.value = res.tool_filter.tools || []
    ElMessage.success('MCP 设置已保存并生效')
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    mcpSaving.value = false
  }
}

// ---------- 路径白名单 ----------
function addWhitelistPath() {
  const p = newPath.value.trim()
  newPath.value = ''
  if (!p) return
  if (!p.startsWith('/') && !p.startsWith('~')) {
    return ElMessage.warning('路径须为绝对路径(以 / 开头,或以 ~/ 表示主目录)')
  }
  if (wl.paths.includes(p)) return ElMessage.warning('路径已存在')
  wl.paths.push(p)
  saveMcpSettings()
}

function removeWhitelistPath(p) {
  wl.paths = wl.paths.filter((x) => x !== p)
  saveMcpSettings()
}

// ---------- 可见主机:按分组勾选 ----------
function isGroupAll(g) {
  return g.hosts.every((h) => vh.ids.includes(h.id))
}
function isGroupSome(g) {
  return g.hosts.some((h) => vh.ids.includes(h.id)) && !isGroupAll(g)
}
function toggleGroup(g) {
  if (isGroupAll(g)) {
    vh.ids = vh.ids.filter((id) => !g.hosts.some((h) => h.id === id))
  } else {
    for (const h of g.hosts) {
      if (!vh.ids.includes(h.id)) vh.ids.push(h.id)
    }
  }
  saveMcpSettings()
}
function toggleHost(h) {
  const i = vh.ids.indexOf(h.id)
  if (i >= 0) vh.ids.splice(i, 1)
  else vh.ids.push(h.id)
  saveMcpSettings()
}

// ---------- 工具权限 ----------
function toggleTool(name) {
  const i = tf.value.indexOf(name)
  if (i >= 0) tf.value.splice(i, 1)
  else tf.value.push(name)
  saveMcpSettings()
}

onMounted(async () => {
  try {
    const res = await mcpGetSettings()
    wl.enabled = !!res.path_whitelist?.enabled
    wl.paths = res.path_whitelist?.paths || []
    vh.enabled = !!res.visible_hosts?.enabled
    vh.ids = res.visible_hosts?.ids || []
    tf.value = res.tool_filter?.tools || []
    mcpLoaded.value = true
  } catch {
    /* 拉取失败保持默认值,保存时后端会校验 */
  }
  try {
    const hosts = await listHosts()
    const byGroup = new Map()
    for (const h of hosts) {
      const g = h.group || ''
      if (!byGroup.has(g)) byGroup.set(g, [])
      byGroup.get(g).push({ id: h.id, name: h.name, host: h.host })
    }
    hostGroups.value = [...byGroup.entries()]
      .sort((a, b) => (a[0] < b[0] ? -1 : 1))
      .map(([group, hs]) => ({ group, hosts: hs }))
  } catch {
    /* 主机列表拉取失败:勾选区留空 */
  }
})

// ---------- 关于:支持信息 ----------
const APP_VERSION = '0.1.0'
const ua = navigator.userAgent
const osInfo = (() => {
  if (/Windows/i.test(ua)) return 'Windows'
  if (/Mac OS X/i.test(ua)) return 'macOS'
  if (/Android/i.test(ua)) return 'Android'
  if (/iPhone|iPad/i.test(ua)) return 'iOS'
  if (/Linux/i.test(ua)) return 'Linux'
  return '未知'
})()
const archInfo = (() => {
  if (/x86_64|x64|wow64/i.test(ua)) return 'x86_64'
  if (/arm64|aarch64/i.test(ua)) return 'arm64'
  return '未知'
})()

const supportItems = [
  { label: '当前版本', value: APP_VERSION },
  { label: '操作系统', value: osInfo },
  { label: '系统架构', value: archInfo },
]

async function copySupportInfo() {
  const text = `版本: ${APP_VERSION}\n操作系统: ${osInfo}\n系统架构: ${archInfo}`
  try {
    await navigator.clipboard.writeText(text)
    ElMessage.success('已复制支持信息')
  } catch {
    ElMessage.error('复制失败')
  }
}
</script>

<template>
  <div class="settings-panel">
    <div class="settings-main">
      <aside class="settings-nav">
        <div
          v-for="m in menus"
          :key="m.key"
          class="settings-nav-item"
          :class="{ active: activeMenu === m.key }"
          @click="activeMenu = m.key"
        >
          {{ m.label }}
        </div>
      </aside>
      <div class="settings-body">
        <div v-if="activeMenu === 'general'" class="settings-pane">
          <div class="settings-section">
            <div class="settings-card-title">主题</div>
            <div class="settings-options">
              <div
                v-for="opt in themeOptions"
                :key="opt.value"
                class="settings-option"
                :class="{ active: settings.theme === opt.value }"
                @click="settings.theme = opt.value"
              >
                <div class="settings-option-title">{{ opt.title }}</div>
                <div class="settings-option-desc">{{ opt.desc }}</div>
              </div>
            </div>
          </div>
          <div class="settings-section">
            <div class="settings-card-title">侧边栏打开方式</div>
            <div class="settings-options">
              <div
                v-for="opt in openModeOptions"
                :key="opt.value"
                class="settings-option"
                :class="{ active: settings.sidebarOpenMode === opt.value }"
                @click="settings.sidebarOpenMode = opt.value"
              >
                <div class="settings-option-title">{{ opt.title }}</div>
                <div class="settings-option-desc">{{ opt.desc }}</div>
              </div>
            </div>
          </div>
        </div>
        <div v-else-if="activeMenu === 'security'" class="settings-pane">
          <div class="settings-section">
            <div class="settings-card-title">修改启动密码</div>
            <div class="settings-card-desc">用于解锁本工具的启动密码,修改后立即生效,当前设备保持解锁。</div>
            <el-form label-position="top" class="security-form" @submit.prevent>
              <el-form-item label="当前密码">
                <el-input
                  v-model="pwdForm.current"
                  type="password"
                  show-password
                  autocomplete="current-password"
                  placeholder="请输入当前密码"
                />
              </el-form-item>
              <el-form-item label="新密码">
                <el-input
                  v-model="pwdForm.next"
                  type="password"
                  show-password
                  autocomplete="new-password"
                  placeholder="至少 4 位"
                />
              </el-form-item>
              <el-form-item label="重复新密码">
                <el-input
                  v-model="pwdForm.repeat"
                  type="password"
                  show-password
                  autocomplete="new-password"
                  placeholder="再次输入新密码"
                />
              </el-form-item>
              <el-form-item>
                <el-button type="primary" :loading="pwdSaving" @click="submitChangePassword">
                  修改密码
                </el-button>
              </el-form-item>
            </el-form>
          </div>
        </div>
        <div v-else-if="activeMenu === 'mcp'" class="settings-pane">
          <div class="settings-section">
            <div class="settings-card-title">路径白名单</div>
            <div class="settings-card-desc">
              开启后,MCP 的 ssh_exec 命令与文件下载仅允许访问下列前缀内的路径;默认关闭,不做路径限制。
            </div>
            <div class="wl-toggle">
              <el-switch v-model="wl.enabled" :loading="mcpSaving" @change="saveMcpSettings" />
              <span>{{ wl.enabled ? '已开启(仅允许白名单内路径)' : '已关闭(不限制路径)' }}</span>
            </div>
            <template v-if="wl.enabled">
              <div class="wl-list">
                <div v-for="p in wl.paths" :key="p" class="wl-item">
                  <span class="wl-path">{{ p }}</span>
                  <el-button size="small" text type="danger" @click="removeWhitelistPath(p)">删除</el-button>
                </div>
                <div v-if="!wl.paths.length" class="wl-empty">暂无路径;不添加任何路径时所有路径访问都会被拒绝。</div>
              </div>
              <div class="wl-add">
                <el-input
                  v-model="newPath"
                  placeholder="如 /var/log/ 或 ~/data"
                  @keyup.enter="addWhitelistPath"
                />
                <el-button type="primary" :loading="mcpSaving" @click="addWhitelistPath">添加</el-button>
              </div>
            </template>
          </div>
          <div class="settings-section">
            <div class="settings-card-title">可见主机</div>
            <div class="settings-card-desc">
              开启后,MCP 仅能看到勾选的主机(可按分组折叠勾选);默认关闭,全部主机可见。未勾选的主机对 MCP 完全不可见。
            </div>
            <div class="wl-toggle">
              <el-switch v-model="vh.enabled" :loading="mcpSaving" @change="saveMcpSettings" />
              <span>{{ vh.enabled ? `已开启(已选 ${vh.ids.length} 台)` : '已关闭(全部主机可见)' }}</span>
            </div>
            <template v-if="vh.enabled">
              <div class="vh-scroll">
                <el-collapse>
                  <el-collapse-item
                    v-for="g in hostGroups"
                    :key="g.group"
                    :name="g.group || '__ungrouped__'"
                  >
                    <template #title>
                      <span class="vh-group-title" @click.stop>
                        <el-checkbox
                          :model-value="isGroupAll(g)"
                          :indeterminate="isGroupSome(g)"
                          @change="toggleGroup(g)"
                        >
                          {{ g.group || '未分组' }}
                        </el-checkbox>
                        <span class="vh-host-addr">{{ g.hosts.length }} 台</span>
                      </span>
                    </template>
                    <div class="vh-hosts">
                      <el-checkbox
                        v-for="h in g.hosts"
                        :key="h.id"
                        :model-value="vh.ids.includes(h.id)"
                        @change="toggleHost(h)"
                      >
                        {{ h.name }}
                        <span class="vh-host-addr">{{ h.host }}</span>
                      </el-checkbox>
                    </div>
                  </el-collapse-item>
                </el-collapse>
                <div v-if="!hostGroups.length" class="wl-empty">没有可用主机。</div>
              </div>
            </template>
          </div>
          <div class="settings-section">
            <div class="settings-card-title">工具权限</div>
            <div class="settings-card-desc">
              工具白名单:仅勾选的工具对 MCP 暴露;全部不勾选时不暴露任何工具。
            </div>
            <div class="wl-toggle">
              <span>已选 {{ tf.length }}/{{ toolCatalog.length }}</span>
            </div>
            <div class="tf-scroll">
              <div class="tf-grid">
                <el-checkbox
                  v-for="t in toolCatalog"
                  :key="t.name"
                  :model-value="tf.includes(t.name)"
                  @change="toggleTool(t.name)"
                >
                  {{ t.label }}
                  <span class="vh-host-addr">{{ t.name }}</span>
                </el-checkbox>
              </div>
              <div v-if="!tf.length" class="wl-empty">未勾选任何工具;此时 MCP 将不暴露任何工具。</div>
            </div>
          </div>
        </div>
        <div v-else-if="activeMenu === 'about'" class="settings-pane">
          <div class="settings-card">
            <div class="settings-card-head">
              <div>
                <div class="settings-card-title">支持信息</div>
                <div class="settings-card-desc">提交 Issue 或寻求帮助时,可复制这些环境信息。</div>
              </div>
              <el-button size="small" @click="copySupportInfo">复制支持信息</el-button>
            </div>
            <div class="support-grid">
              <div v-for="item in supportItems" :key="item.label" class="support-item">
                <div class="support-label">{{ item.label }}</div>
                <div class="support-value">{{ item.value }}</div>
              </div>
            </div>
          </div>
        </div>
      </div>
    </div>
    <footer class="settings-footer">
      <el-button @click="$emit('close')">关闭</el-button>
      <el-button type="primary" disabled>应用</el-button>
      <el-button type="primary" disabled>应用并关闭</el-button>
    </footer>
  </div>
</template>
