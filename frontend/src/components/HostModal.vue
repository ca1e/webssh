<script setup>
import { computed, reactive, ref, watch } from 'vue'
import { ElMessage } from 'element-plus'
import { createHost, listKeys, testHost, updateHost } from '../api'

const props = defineProps({
  modelValue: Boolean,
  host: { type: Object, default: null }, // null = 新建
  hosts: { type: Array, default: () => [] },
})
const emit = defineEmits(['update:modelValue', 'saved'])

const formRef = ref()
const saving = ref(false)
const testing = ref(false)
const keys = ref([])

const form = reactive({
  name: '',
  host: '',
  port: 22,
  conn_type: 'ssh',
  username: 'root',
  auth_type: 'password',
  password: '',
  key_path: '',
  passphrase: '',
  jump_id: '',
  group: '',
  startup_cmd: '',
  follow_ssh_path: false,
})

const isTelnet = computed(() => form.conn_type === 'telnet')

// 校验按连接方式裁剪:telnet 仅 名称/主机/端口
const rules = computed(() => {
  const base = {
    name: [{ required: true, message: '请输入名称', trigger: 'blur' }],
    host: [{ required: true, message: '请输入主机 IP', trigger: 'blur' }],
    port: [{ required: true, message: '请输入端口', trigger: 'blur' }],
  }
  if (!isTelnet.value) {
    base.username = [{ required: true, message: '请输入用户名', trigger: 'blur' }]
    if (form.auth_type === 'key') {
      base.key_path = [{ required: true, message: '请选择或输入私钥路径', trigger: 'change' }]
    }
  }
  return base
})

// 切换连接方式时,端口为另一方式的默认值则同步跟随
watch(isTelnet, (telnet) => {
  if (form.port === (telnet ? 22 : 23)) form.port = telnet ? 23 : 22
})

const isEdit = computed(() => !!props.host)
const jumpOptions = computed(() =>
  props.hosts.filter((h) => !props.host || h.id !== props.host.id)
)
// 已有分组列表,供下拉选择(也可手输新分组)
const groupOptions = computed(() => {
  const set = new Set()
  for (const h of props.hosts) if (h.group) set.add(h.group)
  return [...set].sort()
})

watch(
  () => props.modelValue,
  (v) => {
    if (!v) return
    form.name = props.host?.name || ''
    form.host = props.host?.host || ''
    form.port = props.host?.port || (props.host?.conn_type === 'telnet' ? 23 : 22)
    form.conn_type = props.host?.conn_type || 'ssh'
    form.username = props.host?.username || (props.host?.conn_type === 'telnet' ? '' : 'root')
    form.auth_type = props.host?.auth_type || 'password'
    form.password = ''
    form.key_path = props.host?.key_path || ''
    form.passphrase = ''
    form.jump_id = props.host?.jump_id || ''
    form.group = props.host?.group || ''
    form.startup_cmd = props.host?.startup_cmd || ''
    form.follow_ssh_path = props.host?.follow_ssh_path || false
    formRef.value?.clearValidate()
    loadKeys()
  }
)

async function loadKeys() {
  try {
    keys.value = await listKeys()
  } catch {
    keys.value = []
  }
}

function payload() {
  const base = { ...form, id: props.host?.id || '', port: Number(form.port) || (isTelnet.value ? 23 : 22) }
  if (isTelnet.value) {
    // telnet 无用户/认证概念:清空 SSH 专属字段,避免隐藏表单里预填的
    // 默认值(如新建时 username='root')被提交入库后在列表中显示
    Object.assign(base, {
      username: '',
      auth_type: '',
      password: '',
      key_path: '',
      passphrase: '',
      jump_id: '',
      startup_cmd: '',
      follow_ssh_path: false,
    })
  }
  return base
}

async function doSave() {
  await formRef.value.validate()
  saving.value = true
  try {
    if (isEdit.value) {
      await updateHost(props.host.id, payload())
      ElMessage.success('连接已更新')
    } else {
      await createHost(payload())
      ElMessage.success('连接已保存')
    }
    emit('saved')
    emit('update:modelValue', false)
  } finally {
    saving.value = false
  }
}

async function doTest() {
  testing.value = true
  try {
    const res = await testHost(payload())
    ElMessage.success(`连接成功(${res.elapsed_ms} ms)`)
  } catch (e) {
    ElMessage.error({ message: e.message, duration: 6000, showClose: true })
  } finally {
    testing.value = false
  }
}
</script>

<template>
  <el-dialog
    :model-value="modelValue"
    :title="isEdit ? '编辑连接' : '新建连接'"
    width="520px"
    :close-on-click-modal="false"
    @update:model-value="emit('update:modelValue', $event)"
  >
    <el-form ref="formRef" :model="form" :rules="rules" label-width="90px">
      <el-form-item label="连接方式">
        <el-select v-model="form.conn_type" style="width: 160px">
          <el-option label="SSH" value="ssh" />
          <el-option label="Telnet" value="telnet" />
        </el-select>
      </el-form-item>
      <el-form-item label="名称" prop="name">
        <el-input v-model="form.name" placeholder="例如:生产 Web 服务器" />
      </el-form-item>
      <el-form-item label="主机 IP" prop="host">
        <el-input v-model="form.host" placeholder="例如:192.168.1.10" />
      </el-form-item>
      <el-form-item label="端口" prop="port">
        <el-input-number v-model="form.port" :min="1" :max="65535" controls-position="right" />
      </el-form-item>
      <template v-if="!isTelnet">
      <el-form-item label="用户名" prop="username">
        <el-input v-model="form.username" />
      </el-form-item>
      <el-form-item label="认证类型">
        <el-radio-group v-model="form.auth_type">
          <el-radio-button value="password">密码</el-radio-button>
          <el-radio-button value="key">私钥</el-radio-button>
        </el-radio-group>
      </el-form-item>

      <template v-if="form.auth_type === 'password'">
        <el-form-item label="密码">
          <el-input
            v-model="form.password"
            type="password"
            show-password
            :placeholder="isEdit && host?.has_password ? '已保存,留空保持不变' : '请输入密码'"
          />
        </el-form-item>
      </template>

      <template v-else>
        <el-form-item label="私钥" prop="key_path">
          <el-select
            v-model="form.key_path"
            filterable
            allow-create
            default-first-option
            placeholder="选择本地密钥或输入路径"
            style="width: 100%"
          >
            <el-option
              v-for="k in keys"
              :key="k.path"
              :label="k.name"
              :value="k.path"
            />
          </el-select>
          <div class="muted" style="font-size: 12px; line-height: 1.6">
            来自本机 ~/.ssh,可手动输入其他路径(支持 ~)
          </div>
        </el-form-item>
        <el-form-item label="私钥口令">
          <el-input
            v-model="form.passphrase"
            type="password"
            show-password
            :placeholder="isEdit && host?.has_passphrase ? '已保存,留空保持不变' : '无口令可留空'"
          />
        </el-form-item>
      </template>

      <el-form-item label="跳板机">
        <el-select
          v-model="form.jump_id"
          clearable
          placeholder="可选:经其他已保存主机中转"
          style="width: 100%"
        >
          <el-option
            v-for="h in jumpOptions"
            :key="h.id"
            :label="`${h.name}(${h.host})`"
            :value="h.id"
          />
        </el-select>
      </el-form-item>
      </template>

      <el-form-item label="分组">
        <el-select
          v-model="form.group"
          filterable
          allow-create
          default-first-option
          clearable
          placeholder="可选,如:生产环境"
          style="width: 100%"
        >
          <el-option
            v-for="g in groupOptions"
            :key="g"
            :label="g"
            :value="g"
          />
        </el-select>
        <div class="muted" style="font-size: 12px; line-height: 1.6">
          用于在主机列表中分组归类,留空归入"未分组"
        </div>
      </el-form-item>

      <template v-if="!isTelnet">
      <el-form-item label="启动命令">
        <el-input
          v-model="form.startup_cmd"
          type="textarea"
          :autosize="{ minRows: 1, maxRows: 3 }"
          placeholder="可选,连接后自动执行,如:cd /opt/app && export FOO=bar"
        />
        <div class="muted" style="font-size: 12px; line-height: 1.6">
          连接建立后先执行此命令(无交互终端),执行失败不影响连接
        </div>
      </el-form-item>

      <el-form-item label="Follow SSH path">
        <el-switch v-model="form.follow_ssh_path" />
        <div class="muted" style="font-size: 12px; line-height: 1.6; margin-left: 12px">
          勾选后注入 shell 钩子,SFTP 面板自动跟随终端 cd;该模式不显示 Last login/MOTD 横幅
        </div>
      </el-form-item>
      </template>
    </el-form>

    <template #footer>
      <el-button :loading="testing" @click="doTest">测试连接</el-button>
      <el-button @click="emit('update:modelValue', false)">取消</el-button>
      <el-button type="primary" :loading="saving" @click="doSave">保存</el-button>
    </template>
  </el-dialog>
</template>
