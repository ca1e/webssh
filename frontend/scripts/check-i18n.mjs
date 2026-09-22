// i18n 词表完整性校验(开发期工具,不进构建产物):
//   1) 源码中 t('xxx') 的字面量键必须同时存在于 zh-CN 与 en 词表
//   2) 两份词表键集合必须完全一致
//   3) 模板/脚本中不得残留硬编码中文(词表文件除外)
import { readFileSync, readdirSync, statSync } from 'node:fs'
import { join, dirname } from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'

const root = join(dirname(fileURLToPath(import.meta.url)), '..')
const failed = new Set()

// ---- 加载词表(直接 import,复用运行时数据) ----
const { default: zhCN } = await import(pathToFileURL(join(root, 'src/i18n/zh-CN.js')).href)
const { default: en } = await import(pathToFileURL(join(root, 'src/i18n/en.js')).href)

function flatten(obj, prefix = '', out = {}) {
  for (const [k, v] of Object.entries(obj)) {
    const key = prefix ? `${prefix}.${k}` : k
    if (v && typeof v === 'object') flatten(v, key, out)
    else out[key] = v
  }
  return out
}
const zhKeys = new Set(Object.keys(flatten(zhCN)))
const enKeys = new Set(Object.keys(flatten(en)))

// ---- 收集源文件 ----
function walk(dir, files = []) {
  for (const name of readdirSync(dir)) {
    const p = join(dir, name)
    if (statSync(p).isDirectory()) walk(p, files)
    else if (/\.(vue|js)$/.test(name)) files.push(p)
  }
  return files
}
const srcFiles = walk(join(root, 'src')).filter((p) => !/i18n[/\\](zh-CN|en)\.js$/.test(p))

// ---- 1) 字面量键存在性;同时收集动态键前缀(settings.tool_*)做人工核对 ----
const dynamicPrefixes = new Set()
for (const file of srcFiles) {
  const src = readFileSync(file, 'utf8')
  for (const m of src.matchAll(/\bt\(\s*'([^']+)'\s*[,)]/g)) {
    const key = m[1]
    for (const [name, set] of [['zh-CN', zhKeys], ['en', enKeys]]) {
      if (!set.has(key)) {
        failed.add(`${file}: 键 '${key}' 不存在于 ${name} 词表`)
      }
    }
  }
  for (const m of src.matchAll(/\bt\(\s*'([^']*)'\s*\+/g)) dynamicPrefixes.add(m[1])
}
// 动态键:settings.tool_ + 工具名(与 settings.toolPermDesc 等前缀冲突时需人工核对)
for (const prefix of dynamicPrefixes) {
  const covered = [...zhKeys].filter((k) => k.startsWith(prefix))
  console.log(`动态键前缀 '${prefix}' → 词表内匹配 ${covered.length} 个: ${covered.join(', ')}`)
}

// ---- 2) 两词表键集合一致 ----
for (const k of zhKeys) if (!enKeys.has(k)) failed.add(`en 词表缺少键: ${k}`)
for (const k of enKeys) if (!zhKeys.has(k)) failed.add(`zh-CN 词表缺少键: ${k}`)

// ---- 3) 残留硬编码中文(排除注释行;豁免:i18n 模块、后端协议检测串) ----
for (const file of srcFiles) {
  if (/[/\\]i18n[/\\]/.test(file)) continue // 词表与语言自身名称(简体中文)不受限
  const lines = readFileSync(file, 'utf8').split('\n')
  lines.forEach((line, i) => {
    const noComment = line
      .replace(/\/\/.*$/, '')
      .replace(/\/\*.*?\*\//g, '')
      .replace(/<!--.*?-->/g, '')
    if (/[\u4e00-\u9fff]/.test(noComment)) {
      if (line.includes('信任新密钥')) return // 后端中文错误报文的匹配契约,非界面文案
      failed.add(`${file}:${i + 1}: 疑似残留中文 → ${line.trim().slice(0, 80)}`)
    }
  })
}

if (failed.size) {
  console.error(`\n✗ i18n 校验失败 ${failed.size} 处:`)
  for (const f of failed) console.error('  ' + f)
  process.exit(1)
}
console.log(`✓ i18n 校验通过:${zhKeys.size} 个键,zh-CN/en 完全一致;${srcFiles.length} 个源文件无残留中文`)
