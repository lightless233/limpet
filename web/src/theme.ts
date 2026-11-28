// 界面主题管理：dark | light | system 三种模式，最终生效值始终是 dark 或 light。
// 持久化到 localStorage（key: limpet.theme），默认 dark，避免老用户升级后界面突变。
// 生效值变化时同步 data-theme 属性与 theme-color / color-scheme meta，
// CSS 侧由 style.css 的 :root[data-theme="light"] 分支切换变量、color-scheme 与 scrollbar-color。
// index.html 里的内联脚本会先用同一套规则提前设置 data-theme，防止首帧闪烁；
// 这里初始化后重新对齐，并接管跨标签页（storage）与跟随系统（matchMedia）的实时联动。
import { ref } from 'vue'

export type ThemeMode = 'dark' | 'light' | 'system'
export type ResolvedTheme = 'dark' | 'light'

const STORAGE_KEY = 'limpet.theme'
const META_COLOR = { dark: '#121417', light: '#eae8e2' } as const

const media = matchMedia('(prefers-color-scheme: dark)')

function readStoredMode(): ThemeMode {
  try {
    const value = localStorage.getItem(STORAGE_KEY)
    return value === 'light' || value === 'system' ? value : 'dark'
  } catch {
    return 'dark'
  }
}

function resolve(mode: ThemeMode): ResolvedTheme {
  return mode === 'system' ? (media.matches ? 'dark' : 'light') : mode
}

export const themeMode = ref<ThemeMode>(readStoredMode())
export const resolvedTheme = ref<ResolvedTheme>(resolve(themeMode.value))

function setMeta(name: string, content: string) {
  let meta = document.querySelector(`meta[name="${name}"]`)
  if (!meta) {
    meta = document.createElement('meta')
    meta.setAttribute('name', name)
    document.head.appendChild(meta)
  }
  meta.setAttribute('content', content)
}

function apply() {
  document.documentElement.dataset.theme = resolvedTheme.value
  setMeta('theme-color', META_COLOR[resolvedTheme.value])
  setMeta('color-scheme', resolvedTheme.value)
}

function recompute() {
  resolvedTheme.value = resolve(themeMode.value)
  apply()
}

// 三态切换入口：终端工具弹窗的三态控件使用。
export function setThemeMode(mode: ThemeMode) {
  themeMode.value = mode
  try {
    localStorage.setItem(STORAGE_KEY, mode)
  } catch { /* 当前页面仍生效，仅不持久化。 */ }
  recompute()
}

// 一键在当前生效的深/浅色之间切换（顶栏图标按钮使用，脱离 system 模式）。
export function toggleTheme() {
  setThemeMode(resolvedTheme.value === 'dark' ? 'light' : 'dark')
}

let started = false

export function initTheme() {
  if (started) return
  started = true
  apply()
  media.addEventListener('change', () => {
    if (themeMode.value === 'system') recompute()
  })
  // storage 事件只在其他标签页触发，实现跨标签页同步；key 为 null 表示被清空。
  window.addEventListener('storage', (e) => {
    if (e.key !== STORAGE_KEY && e.key !== null) return
    const next = readStoredMode()
    if (next !== themeMode.value) {
      themeMode.value = next
      recompute()
    }
  })
}
