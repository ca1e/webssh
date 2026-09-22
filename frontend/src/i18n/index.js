// i18n:基于 vue-i18n(legacy: false Composition 模式)。
// 持久化配置唯一落在 settings.language(localStorage),i18n locale 跟随它;
// composer 的 locale 是响应式 ref,模板/渲染函数调用 t() 会把依赖收进当前
// 渲染 effect,切换语言即全界面热重载,无需刷新页面。
import { watch } from 'vue'
import { createI18n } from 'vue-i18n'
import { settings } from '../settings'
import zhCN from './zh-CN'
import en from './en'

export const i18n = createI18n({
  legacy: false,
  locale: settings.language,
  fallbackLocale: 'zh-CN',
  messages: { 'zh-CN': zhCN, en },
  // 缺失键静默回退简体中文;词表完整性由脚本校验,不在控制台刷警告
  missingWarn: false,
  fallbackWarn: false,
})

// 供组件与非组件模块(api.js)统一使用的翻译入口,委托 vue-i18n 全局 composer
export const t = (...args) => i18n.global.t(...args)

// 语言选项(设置页通用页),{ value, flag, label } 形态:
// flag 为国旗 emoji,label 固定用语言自身名称,均不随界面语言翻译
export const localeOptions = [
  { value: 'zh-CN', flag: '🇨🇳', label: '简体中文' },
  { value: 'en', flag: '🇺🇸', label: 'English' },
]

// 语言变更统一入口:写 settings 即联动 i18n locale 与 <html lang>/页面标题
watch(
  () => settings.language,
  (lang) => {
    i18n.global.locale.value = lang
    document.documentElement.lang = lang
    document.title = t('app.pageTitle')
  },
  { immediate: true }
)
