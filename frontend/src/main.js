import { createApp } from 'vue'
import ElementPlus from 'element-plus'
import 'element-plus/dist/index.css'
import 'element-plus/theme-chalk/dark/css-vars.css'
import './style.css'
import App from './App.vue'
import { i18n } from './i18n'

// Element Plus 组件库自身文案(弹窗按钮/日期选择器等)由 App.vue 的
// el-config-provider 按界面语言动态提供,此处不再固定 locale
createApp(App).use(ElementPlus).use(i18n).mount('#app')
