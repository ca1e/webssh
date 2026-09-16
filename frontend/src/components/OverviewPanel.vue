<script setup>
// 空标签页时的配置概览:主机/连接统计卡片 + 连接类型分布条形图。
// 主机维度统计来自 props.hosts;"当前已连接"以服务端会话(/api/sessions)为准,
// 多浏览器页面共享同一后端会话列表,组件仅在空态挂载,重挂载即刷新。
import { computed, onMounted, ref, watch } from 'vue'
import { Connection, Link, Monitor, Refresh, Right, Cpu } from '@element-plus/icons-vue'
import { authState, listSessions } from '../api'

const props = defineProps({
  hosts: { type: Array, default: () => [] },
})
defineEmits(['create'])

const sessions = ref(null) // null = 尚未加载成功
const loading = ref(false)

async function refresh() {
  if (authState.locked) return // 锁屏期间会话接口必然 401,不做无谓请求
  loading.value = true
  try {
    sessions.value = await listSessions()
  } catch {
    sessions.value = null // 失败时显示占位符,由刷新按钮兜底
  } finally {
    loading.value = false
  }
}
onMounted(refresh)
// 组件可能在锁屏期间就已挂载(onMounted 被跳过);解锁后补拉一次会话统计
watch(() => authState.locked, (locked) => { if (!locked) refresh() })

const isTelnet = (h) => h.conn_type === 'telnet'
const connKindOf = (t) => (t === 'telnet' ? 'telnet' : 'ssh')

const total = computed(() => props.hosts.length)
const sshCount = computed(() => props.hosts.filter((h) => !isTelnet(h)).length)
const telnetCount = computed(() => props.hosts.filter(isTelnet).length)
const authKeyCount = computed(() => props.hosts.filter((h) => h.auth_type === 'key').length)
const authPassCount = computed(() => total.value - authKeyCount.value)
const jumpCount = computed(() => props.hosts.filter((h) => h.jump_id).length)
const groupCount = computed(() => new Set(props.hosts.map((h) => h.group).filter(Boolean)).size)

const connected = computed(() => (sessions.value ? sessions.value.length : null))
const connSsh = computed(
  () => (sessions.value ? sessions.value.filter((s) => connKindOf(s.conn_type) === 'ssh').length : null)
)
const connTelnet = computed(
  () => (sessions.value ? sessions.value.filter((s) => connKindOf(s.conn_type) === 'telnet').length : null)
)

// 主机分布条形图各段:数量为 0 的类型不显示
const distBars = computed(() =>
  [
    { name: 'SSH', n: sshCount.value, cls: 'ssh' },
    { name: 'Telnet', n: telnetCount.value, cls: 'telnet' },
  ].filter((b) => b.n > 0)
)
</script>

<template>
  <!-- 首启无主机:只显示引导卡,不渲染一排 0 -->
  <div v-if="!total" class="ov-guide">
    <el-icon :size="42" class="ov-guide-icon"><Monitor /></el-icon>
    <div class="ov-guide-title">还没有保存任何连接</div>
    <div class="ov-guide-sub">创建第一个主机,或在顶部点击「新建连接」</div>
    <el-button type="primary" :icon="Right" @click="$emit('create')">新建连接</el-button>
  </div>

  <div v-else class="ov">
    <div class="ov-cards">
      <div class="ov-card">
        <div class="ov-card-head"><el-icon><Monitor /></el-icon><span>主机总数</span></div>
        <div class="ov-num">{{ total }}</div>
        <div class="ov-sub">密码 {{ authPassCount }} · 密钥 {{ authKeyCount }}</div>
      </div>
      <div class="ov-card">
        <div class="ov-card-head">
          <el-icon><Link /></el-icon><span>当前已连接</span>
          <el-button class="ov-refresh" text :icon="Refresh" :loading="loading" size="small" @click="refresh" />
        </div>
        <div class="ov-num" :class="{ 'ov-num-muted': connected === null }">
          {{ connected === null ? '—' : connected }}
        </div>
        <div class="ov-sub" v-if="connected !== null">SSH {{ connSsh }} · Telnet {{ connTelnet }}</div>
        <div class="ov-sub" v-else>会话状态获取失败,点击刷新</div>
      </div>
      <div class="ov-card">
        <div class="ov-card-head"><el-icon><Connection /></el-icon><span>SSH 主机</span></div>
        <div class="ov-num">{{ sshCount }}</div>
        <div class="ov-sub">经跳板机 {{ jumpCount }}</div>
      </div>
      <div class="ov-card">
        <div class="ov-card-head"><el-icon><Cpu /></el-icon><span>Telnet 主机</span></div>
        <div class="ov-num">{{ telnetCount }}</div>
        <div class="ov-sub">{{ groupCount ? `分组 ${groupCount} 个` : '未使用分组' }}</div>
      </div>
    </div>

    <div class="ov-dist">
      <div class="ov-dist-title">连接类型分布</div>
      <div class="ov-dist-bar">
        <div
          v-for="b in distBars"
          :key="b.cls"
          class="ov-dist-seg"
          :class="b.cls"
          :style="{ width: (b.n / total) * 100 + '%' }"
        >
          <span v-if="b.n / total > 0.14">{{ b.name }} {{ b.n }}</span>
        </div>
      </div>
      <div class="ov-dist-legend">
        <span v-for="b in distBars" :key="b.cls">
          <i class="ov-dot" :class="b.cls" />{{ b.name }} {{ b.n }} 台({{ ((b.n / total) * 100).toFixed(0) }}%)
        </span>
      </div>
    </div>

    <div class="ov-hint">点击左侧主机的「连接」开始会话(可右键主机进行重命名、复制等操作)</div>
  </div>
</template>

<style scoped>
.ov {
  padding: 48px 28px 24px;
  max-width: 980px;
  margin: 0 auto;
}

/* ---------- 顶部数字卡 ---------- */
.ov-cards {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(170px, 1fr));
  gap: 14px;
}
.ov-card {
  background: var(--bg-card);
  border: 1px solid var(--border);
  border-radius: 10px;
  padding: 16px 18px 14px;
  min-width: 0;
}
.ov-card-head {
  display: flex;
  align-items: center;
  gap: 6px;
  color: var(--text-secondary);
  font-size: 13px;
}
.ov-card-head .el-icon {
  color: var(--el-color-primary);
}
.ov-refresh {
  margin-left: auto;
  color: var(--text-secondary);
}
.ov-num {
  font-size: 32px;
  font-weight: 600;
  line-height: 1.35;
  color: var(--text-strong);
  font-variant-numeric: tabular-nums;
}
.ov-num-muted {
  color: var(--text-disabled);
}
.ov-sub {
  font-size: 12px;
  color: var(--text-secondary);
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}

/* ---------- 类型分布条形图(纯 CSS,不引图表库) ---------- */
.ov-dist {
  margin-top: 22px;
  background: var(--bg-card);
  border: 1px solid var(--border);
  border-radius: 10px;
  padding: 16px 18px;
}
.ov-dist-title {
  font-size: 13px;
  color: var(--text-secondary);
  margin-bottom: 10px;
}
.ov-dist-bar {
  display: flex;
  height: 22px;
  border-radius: 6px;
  overflow: hidden;
  background: var(--bg-soft);
}
.ov-dist-seg {
  display: flex;
  align-items: center;
  justify-content: center;
  min-width: 4px;
  transition: width 0.3s;
}
.ov-dist-seg span {
  font-size: 11px;
  color: #fff;
  white-space: nowrap;
}
.ov-dist-seg.ssh { background: var(--el-color-primary); }
.ov-dist-seg.telnet { background: #e6a23c; }
.ov-dist-legend {
  display: flex;
  gap: 18px;
  margin-top: 10px;
  font-size: 12px;
  color: var(--text-regular);
}
.ov-dot {
  display: inline-block;
  width: 8px;
  height: 8px;
  border-radius: 50%;
  margin-right: 5px;
}
.ov-dot.ssh { background: var(--el-color-primary); }
.ov-dot.telnet { background: #e6a23c; }

.ov-hint {
  margin-top: 20px;
  text-align: center;
  font-size: 12px;
  color: var(--text-disabled);
}

/* ---------- 首启引导卡 ---------- */
.ov-guide {
  margin: 96px auto 0;
  max-width: 420px;
  background: var(--bg-card);
  border: 1px solid var(--border);
  border-radius: 12px;
  padding: 40px 32px;
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 10px;
}
.ov-guide-icon {
  color: var(--el-color-primary);
  margin-bottom: 4px;
}
.ov-guide-title {
  font-size: 16px;
  font-weight: 600;
  color: var(--text-strong);
}
.ov-guide-sub {
  font-size: 13px;
  color: var(--text-secondary);
  margin-bottom: 10px;
}
</style>
