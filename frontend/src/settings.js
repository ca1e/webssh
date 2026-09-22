import { computed, reactive, ref, watch } from 'vue'

// 应用级设置:跨组件共享,变更即时生效并持久化到 localStorage
const STORAGE_KEY = 'webssh-settings'

function load() {
  try {
    return JSON.parse(localStorage.getItem(STORAGE_KEY) || '{}')
  } catch {
    return {}
  }
}

const saved = load()

export const settings = reactive({
  // 主机列表打开方式:dblclick=双击打开,click=单击打开
  sidebarOpenMode: saved.sidebarOpenMode === 'click' ? 'click' : 'dblclick',
  // 主题:light=浅色(默认,即原配色),dark=深色,system=跟随系统
  theme: ['light', 'dark', 'system'].includes(saved.theme) ? saved.theme : 'light',
  // 界面语言:zh-CN=简体中文(默认),en=英文;由 src/i18n 消费,切换即热重载界面文案
  language: ['zh-CN', 'en'].includes(saved.language) ? saved.language : 'zh-CN',
})

watch(settings, (v) => {
  localStorage.setItem(STORAGE_KEY, JSON.stringify(v))
})

// 跟随系统:读取真实系统深浅色偏好;浏览器不支持时保持浅色(原配色)
const darkQuery =
  typeof window.matchMedia === 'function'
    ? window.matchMedia('(prefers-color-scheme: dark)')
    : null
const systemPrefersDark = ref(darkQuery ? darkQuery.matches : false)
if (darkQuery) {
  darkQuery.addEventListener('change', (e) => {
    systemPrefersDark.value = e.matches
  })
}

export const isDark = computed(
  () => settings.theme === 'dark' || (settings.theme === 'system' && !!darkQuery && systemPrefersDark.value)
)

// 主题应用到根元素:自身变量与 Element Plus 深色变量都挂在 html.dark 下
watch(
  isDark,
  (dark) => {
    document.documentElement.classList.toggle('dark', dark)
  },
  { immediate: true }
)
