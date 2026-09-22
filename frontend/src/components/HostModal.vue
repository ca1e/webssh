<script setup>
import { computed, reactive, ref, watch } from 'vue'
import { ElMessage } from 'element-plus'
import { createHost, listKeys, testHost, updateHost } from '../api'
import { t } from '../i18n'

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
    name: [{ required: true, message: t('host.ruleName'), trigger: 'blur' }],
    host: [{ required: true, message: t('host.ruleHost'), trigger: 'blur' }],
    port: [{ required: true, message: t('host.rulePort'), trigger: 'blur' }],
  }
  if (!isTelnet.value) {
    base.username = [{ required: true, message: t('host.ruleUsername'), trigger: 'blur' }]
    if (form.auth_type === 'key') {
      base.key_path = [{ required: true, message: t('host.ruleKeyPath'), trigger: 'change' }]
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
      ElMessage.success(t('host.updated'))
    } else {
      await createHost(payload())
      ElMessage.success(t('host.saved'))
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
    ElMessage.success(t('host.testOk', { ms: res.elapsed_ms }))
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
    :title="isEdit ? t('host.editTitle') : t('host.createTitle')"
    width="520px"
    :close-on-click-modal="false"
    @update:model-value="emit('update:modelValue', $event)"
  >
    <el-form ref="formRef" :model="form" :rules="rules" label-width="90px">
      <el-form-item :label="t('host.connType')">
        <el-select v-model="form.conn_type" style="width: 160px">
          <el-option label="SSH" value="ssh" />
          <el-option label="Telnet" value="telnet" />
        </el-select>
      </el-form-item>
      <el-form-item :label="t('host.nameLabel')" prop="name">
        <el-input v-model="form.name" :placeholder="t('host.namePlaceholder')" />
      </el-form-item>
      <el-form-item :label="t('host.hostIp')" prop="host">
        <el-input v-model="form.host" :placeholder="t('host.ipPlaceholder')" />
      </el-form-item>
      <el-form-item :label="t('host.port')" prop="port">
        <el-input-number v-model="form.port" :min="1" :max="65535" controls-position="right" />
      </el-form-item>
      <template v-if="!isTelnet">
      <el-form-item :label="t('host.username')" prop="username">
        <el-input v-model="form.username" />
      </el-form-item>
      <el-form-item :label="t('host.authType')">
        <el-radio-group v-model="form.auth_type">
          <el-radio-button value="password">{{ t('host.password') }}</el-radio-button>
          <el-radio-button value="key">{{ t('host.privateKey') }}</el-radio-button>
        </el-radio-group>
      </el-form-item>

      <template v-if="form.auth_type === 'password'">
        <el-form-item :label="t('host.password')">
          <el-input
            v-model="form.password"
            type="password"
            show-password
            :placeholder="isEdit && host?.has_password ? t('host.savedKeepEmpty') : t('host.pwdPlaceholder')"
          />
        </el-form-item>
      </template>

      <template v-else>
        <el-form-item :label="t('host.privateKey')" prop="key_path">
          <el-select
            v-model="form.key_path"
            filterable
            allow-create
            default-first-option
            :placeholder="t('host.keyPlaceholder')"
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
            {{ t('host.keyHint') }}
          </div>
        </el-form-item>
        <el-form-item :label="t('host.passphrase')">
          <el-input
            v-model="form.passphrase"
            type="password"
            show-password
            :placeholder="isEdit && host?.has_passphrase ? t('host.savedKeepEmpty') : t('host.passphrasePlaceholder')"
          />
        </el-form-item>
      </template>

      <el-form-item :label="t('host.jump')">
        <el-select
          v-model="form.jump_id"
          clearable
          :placeholder="t('host.jumpPlaceholder')"
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

      <el-form-item :label="t('host.group')">
        <el-select
          v-model="form.group"
          filterable
          allow-create
          default-first-option
          clearable
          :placeholder="t('host.groupPlaceholder')"
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
          {{ t('host.groupHint') }}
        </div>
      </el-form-item>

      <template v-if="!isTelnet">
      <el-form-item :label="t('host.startupCmd')">
        <el-input
          v-model="form.startup_cmd"
          type="textarea"
          :autosize="{ minRows: 1, maxRows: 3 }"
          :placeholder="t('host.startupPlaceholder')"
        />
        <div class="muted" style="font-size: 12px; line-height: 1.6">
          {{ t('host.startupHint') }}
        </div>
      </el-form-item>

      <el-form-item label="Follow SSH path">
        <el-switch v-model="form.follow_ssh_path" />
        <div class="muted" style="font-size: 12px; line-height: 1.6; margin-left: 12px">
          {{ t('host.followHint') }}
        </div>
      </el-form-item>
      </template>
    </el-form>

    <template #footer>
      <el-button :loading="testing" @click="doTest">{{ t('host.test') }}</el-button>
      <el-button @click="emit('update:modelValue', false)">{{ t('common.cancel') }}</el-button>
      <el-button type="primary" :loading="saving" @click="doSave">{{ t('common.save') }}</el-button>
    </template>
  </el-dialog>
</template>
