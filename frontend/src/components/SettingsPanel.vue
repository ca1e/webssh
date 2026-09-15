<script setup>
import { reactive, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { settings } from '../settings'
import { authChangePassword } from '../api'

defineEmits(['close'])

// 左侧导航项;右侧面板按 activeMenu 渲染
const menus = [
  { key: 'general', label: '通用' },
  { key: 'security', label: '安全' },
  { key: 'ai', label: 'AI' },
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
        <div v-else-if="activeMenu === 'ai'" class="settings-pane" />
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
