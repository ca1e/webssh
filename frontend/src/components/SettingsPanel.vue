<script setup>
import { computed, reactive, ref, onMounted } from 'vue'
import { ElMessage } from 'element-plus'
import { DocumentCopy } from '@element-plus/icons-vue'
import { settings } from '../settings'
import { authChangePassword, listHosts, mcpGetSettings, mcpSaveSettings } from '../api'
import { t, localeOptions } from '../i18n'

defineEmits(['close'])

// 左侧导航项;右侧面板按 activeMenu 渲染(标题走 i18n)
const menus = [
  { key: 'general' },
  { key: 'security' },
  { key: 'mcp' },
  { key: 'about' },
]
const activeMenu = ref('general')

// ---------- 安全:修改启动密码 ----------
const pwdForm = reactive({ current: '', next: '', repeat: '' })
const pwdSaving = ref(false)

async function submitChangePassword() {
  if (!pwdForm.current) return ElMessage.warning(t('settings.warnCurrentPwd'))
  if (pwdForm.next.length < 4) return ElMessage.warning(t('settings.warnPwdShort'))
  if (pwdForm.next !== pwdForm.repeat) return ElMessage.warning(t('settings.warnPwdMismatch'))
  if (pwdForm.next === pwdForm.current) return ElMessage.warning(t('settings.warnPwdSame'))
  pwdSaving.value = true
  try {
    await authChangePassword(pwdForm.current, pwdForm.next)
    ElMessage.success(t('settings.pwdChanged'))
    pwdForm.current = pwdForm.next = pwdForm.repeat = ''
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    pwdSaving.value = false
  }
}

// ---------- 通用:主题 ----------
const themeOptions = [
  { value: 'light', titleKey: 'settings.themeLight', descKey: 'settings.themeLightDesc' },
  { value: 'dark', titleKey: 'settings.themeDark', descKey: 'settings.themeDarkDesc' },
  { value: 'system', titleKey: 'settings.themeSystem', descKey: 'settings.themeSystemDesc' },
]

// ---------- 通用:侧边栏打开方式 ----------
const openModeOptions = [
  { value: 'click', titleKey: 'settings.openModeClick', descKey: 'settings.openModeClickDesc' },
  { value: 'dblclick', titleKey: 'settings.openModeDblclick', descKey: 'settings.openModeDblclickDesc' },
]

// ---------- MCP:接入 / 管理 tab ----------
const mcpTab = ref('access')

// 接入配置 JSON:同源 /mcp 端点;口令不出服务端,只给占位符让用户自行替换
// (WEBSSH_MCP_TOKEN 环境变量 > mcp.json 的 token 字段 > 锁屏密码)
const accessConfigJson = computed(() =>
  JSON.stringify(
    {
      mcpServers: {
        webssh: {
          url: `${window.location.origin}/mcp`,
          headers: { Authorization: `Bearer ${t('settings.accessTokenHint')}` },
        },
      },
    },
    null,
    2,
  ),
)

async function copyAccessConfig() {
  try {
    await navigator.clipboard.writeText(accessConfigJson.value)
    ElMessage.success(t('settings.accessCopied'))
  } catch {
    ElMessage.error(t('settings.copyFailed'))
  }
}

// ---------- MCP 管理:路径白名单 / 可见主机 / 工具权限 ----------
const wl = reactive({ enabled: false, paths: [] })
const vh = reactive({ enabled: false, ids: [] })
const tf = ref([]) // 工具白名单:显式列表,空 = 不暴露任何工具
const mcpLoaded = ref(false)
const newPath = ref('')
const mcpSaving = ref(false)
// 全部主机(按分组组织)与工具目录
const hostGroups = ref([])
const toolNames = [
  'ssh_list_hosts',
  'ssh_host_info',
  'ssh_test_connection',
  'ssh_create_host',
  'ssh_edit_host',
  'ssh_duplicate_host',
  'ssh_delete_host',
  'ssh_list_sessions',
  'ssh_exec',
  'ssh_download_file',
  'ssh_system_info',
  'ssh_cpu_memory',
  'ssh_disk_usage',
  'ssh_network_status',
  'ssh_service_status',
  'ssh_docker_status',
  'ssh_k8s_status',
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
    ElMessage.success(t('settings.mcpSaved'))
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
    return ElMessage.warning(t('settings.warnPathAbsolute'))
  }
  if (wl.paths.includes(p)) return ElMessage.warning(t('settings.warnPathExists'))
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

// ---------- 工具权限:单个工具勾选 ----------
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
  return t('common.unknown')
})()
const archInfo = (() => {
  if (/x86_64|x64|wow64/i.test(ua)) return 'x86_64'
  if (/arm64|aarch64/i.test(ua)) return 'arm64'
  return t('common.unknown')
})()

const supportItems = computed(() => [
  { label: t('settings.version'), value: APP_VERSION },
  { label: t('settings.os'), value: osInfo },
  { label: t('settings.arch'), value: archInfo },
])

async function copySupportInfo() {
  const text = t('settings.supportText', { v: APP_VERSION, o: osInfo, a: archInfo })
  try {
    await navigator.clipboard.writeText(text)
    ElMessage.success(t('settings.copied'))
  } catch {
    ElMessage.error(t('settings.copyFailed'))
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
          {{ t('settings.' + m.key) }}
        </div>
      </aside>
      <div class="settings-body">
        <div v-if="activeMenu === 'general'" class="settings-pane">
          <div class="settings-section">
            <div class="settings-card-title">{{ t('settings.theme') }}</div>
            <div class="settings-options">
              <div
                v-for="opt in themeOptions"
                :key="opt.value"
                class="settings-option"
                :class="{ active: settings.theme === opt.value }"
                @click="settings.theme = opt.value"
              >
                <div class="settings-option-title">{{ t(opt.titleKey) }}</div>
                <div class="settings-option-desc">{{ t(opt.descKey) }}</div>
              </div>
            </div>
          </div>
          <div class="settings-section">
            <div class="settings-card-title">{{ t('settings.language') }}</div>
            <div class="settings-card-desc">{{ t('settings.languageDesc') }}</div>
            <el-select
              v-model="settings.language"
              class="language-select"
              :aria-label="t('settings.language')"
            >
              <el-option
                v-for="opt in localeOptions"
                :key="opt.value"
                :value="opt.value"
                :label="opt.label"
              >
                <span class="language-option">
                  <span class="language-flag">{{ opt.flag }}</span>
                  <span>{{ opt.label }}</span>
                </span>
              </el-option>
            </el-select>
          </div>
          <div class="settings-section">
            <div class="settings-card-title">{{ t('settings.openMode') }}</div>
            <div class="settings-options">
              <div
                v-for="opt in openModeOptions"
                :key="opt.value"
                class="settings-option"
                :class="{ active: settings.sidebarOpenMode === opt.value }"
                @click="settings.sidebarOpenMode = opt.value"
              >
                <div class="settings-option-title">{{ t(opt.titleKey) }}</div>
                <div class="settings-option-desc">{{ t(opt.descKey) }}</div>
              </div>
            </div>
          </div>
        </div>
        <div v-else-if="activeMenu === 'security'" class="settings-pane">
          <div class="settings-section">
            <div class="settings-card-title">{{ t('settings.changePwd') }}</div>
            <div class="settings-card-desc">{{ t('settings.changePwdDesc') }}</div>
            <el-form label-position="top" class="security-form" @submit.prevent>
              <el-form-item :label="t('settings.currentPwd')">
                <el-input
                  v-model="pwdForm.current"
                  type="password"
                  show-password
                  autocomplete="current-password"
                  :placeholder="t('settings.currentPwdPlaceholder')"
                />
              </el-form-item>
              <el-form-item :label="t('settings.newPwd')">
                <el-input
                  v-model="pwdForm.next"
                  type="password"
                  show-password
                  autocomplete="new-password"
                  :placeholder="t('settings.pwdMinHint')"
                />
              </el-form-item>
              <el-form-item :label="t('settings.repeatPwd')">
                <el-input
                  v-model="pwdForm.repeat"
                  type="password"
                  show-password
                  autocomplete="new-password"
                  :placeholder="t('settings.repeatPwdPlaceholder')"
                />
              </el-form-item>
              <el-form-item>
                <el-button type="primary" :loading="pwdSaving" @click="submitChangePassword">
                  {{ t('settings.changePwdBtn') }}
                </el-button>
              </el-form-item>
            </el-form>
          </div>
        </div>
        <div v-else-if="activeMenu === 'mcp'" class="settings-pane">
          <div class="settings-options mcp-mode">
            <div
              class="settings-option"
              :class="{ active: mcpTab === 'access' }"
              @click="mcpTab = 'access'"
            >
              <div class="settings-option-title">{{ t('settings.mcpAccess') }}</div>
              <div class="settings-option-desc">{{ t('settings.mcpAccessDesc') }}</div>
            </div>
            <div
              class="settings-option"
              :class="{ active: mcpTab === 'manage' }"
              @click="mcpTab = 'manage'"
            >
              <div class="settings-option-title">{{ t('settings.mcpManage') }}</div>
              <div class="settings-option-desc">{{ t('settings.mcpManageDesc') }}</div>
            </div>
          </div>
          <template v-if="mcpTab === 'access'">
            <div class="settings-section">
              <div class="settings-card-title">{{ t('settings.accessTitle') }}</div>
              <div class="settings-card-desc">{{ t('settings.accessDesc') }}</div>
              <div class="access-config">
                <el-input
                  :model-value="accessConfigJson"
                  type="textarea"
                  :rows="10"
                  readonly
                />
                <el-button
                  class="access-copy"
                  size="small"
                  :icon="DocumentCopy"
                  :title="t('common.copy')"
                  :aria-label="t('common.copy')"
                  @click="copyAccessConfig"
                />
              </div>
            </div>
          </template>
          <template v-else>
            <div class="settings-section">
              <div class="settings-card-title">{{ t('settings.pathWhitelist') }}</div>
              <div class="settings-card-desc">{{ t('settings.pathWhitelistDesc') }}</div>
              <div class="wl-toggle">
                <el-switch v-model="wl.enabled" :loading="mcpSaving" @change="saveMcpSettings" />
                <span>{{ wl.enabled ? t('settings.wlOn') : t('settings.wlOff') }}</span>
              </div>
              <template v-if="wl.enabled">
                <div class="wl-list">
                  <div v-for="p in wl.paths" :key="p" class="wl-item">
                    <span class="wl-path">{{ p }}</span>
                    <el-button size="small" text type="danger" @click="removeWhitelistPath(p)">{{ t('common.delete') }}</el-button>
                  </div>
                  <div v-if="!wl.paths.length" class="wl-empty">{{ t('settings.wlEmpty') }}</div>
                </div>
                <div class="wl-add">
                  <el-input
                    v-model="newPath"
                    :placeholder="t('settings.wlPlaceholder')"
                    @keyup.enter="addWhitelistPath"
                  />
                  <el-button type="primary" :loading="mcpSaving" @click="addWhitelistPath">{{ t('common.add') }}</el-button>
                </div>
              </template>
            </div>
            <div class="settings-section">
              <div class="settings-card-title">{{ t('settings.visibleHosts') }}</div>
              <div class="settings-card-desc">{{ t('settings.visibleHostsDesc') }}</div>
              <div class="wl-toggle">
                <el-switch v-model="vh.enabled" :loading="mcpSaving" @change="saveMcpSettings" />
                <span>{{ vh.enabled ? t('settings.vhOn', { n: vh.ids.length }) : t('settings.vhOff') }}</span>
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
                            {{ g.group || t('host.ungrouped') }}
                          </el-checkbox>
                          <span class="vh-host-addr">{{ t('settings.hostCount', { n: g.hosts.length }) }}</span>
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
                  <div v-if="!hostGroups.length" class="wl-empty">{{ t('settings.noHosts') }}</div>
                </div>
              </template>
            </div>
            <div class="settings-section">
              <div class="settings-card-title">{{ t('settings.toolPerm') }}</div>
              <div class="settings-card-desc">{{ t('settings.toolPermDesc') }}</div>
              <div class="wl-toggle">
                <span>{{ t('settings.selectedCount', { a: tf.length, b: toolNames.length }) }}</span>
              </div>
              <div class="tf-scroll">
                <div class="tf-grid">
                  <el-checkbox
                    v-for="name in toolNames"
                    :key="name"
                    :model-value="tf.includes(name)"
                    @change="toggleTool(name)"
                  >
                    {{ t('settings.tool_' + name) }}
                    <span class="vh-host-addr">{{ name }}</span>
                  </el-checkbox>
                </div>
                <div v-if="!tf.length" class="wl-empty">{{ t('settings.noToolsSelected') }}</div>
              </div>
            </div>
          </template>
        </div>
        <div v-else-if="activeMenu === 'about'" class="settings-pane">
          <div class="settings-card">
            <div class="settings-card-head">
              <div>
                <div class="settings-card-title">{{ t('settings.supportInfo') }}</div>
                <div class="settings-card-desc">{{ t('settings.supportInfoDesc') }}</div>
              </div>
              <el-button size="small" @click="copySupportInfo">{{ t('settings.copySupport') }}</el-button>
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
      <el-button @click="$emit('close')">{{ t('common.close') }}</el-button>
    </footer>
  </div>
</template>
