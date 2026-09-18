import { nextTick, onBeforeUnmount, onMounted, unref } from 'vue'

// 菜单与视口边缘保持的最小间距
const EDGE = 8

// 右键菜单通用逻辑:传入组件的菜单 reactive 状态({ show, x, y, ...附加字段 }),
// open() 先按鼠标坐标显示,渲染完成后实测菜单尺寸,越过视口右/下边缘时
// 向左/向上翻转(边缘贴光标),保证菜单始终完整可见
export function useContextMenu(menu) {
  async function open(x, y, menuRef) {
    menu.x = x
    menu.y = y
    menu.show = true
    await nextTick() // 等菜单渲染完成才能量尺寸;nextTick 先于绘制,不会闪烁
    const el = unref(menuRef)
    if (!el) return
    const rect = el.getBoundingClientRect()
    if (!rect.width || !rect.height) return
    if (menu.x + rect.width > window.innerWidth - EDGE) {
      menu.x = Math.max(EDGE, x - rect.width) // 右侧放不下 → 向左展开
    }
    if (menu.y + rect.height > window.innerHeight - EDGE) {
      menu.y = Math.max(EDGE, y - rect.height) // 下方放不下 → 向上展开
    }
  }

  function close() {
    menu.show = false
  }

  function onAutoClose() {
    close()
  }

  onMounted(() => {
    window.addEventListener('click', onAutoClose)
    window.addEventListener('resize', onAutoClose)
    window.addEventListener('blur', onAutoClose)
  })

  onBeforeUnmount(() => {
    window.removeEventListener('click', onAutoClose)
    window.removeEventListener('resize', onAutoClose)
    window.removeEventListener('blur', onAutoClose)
  })

  return { open, close }
}
