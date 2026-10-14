import { argbFromHex, hexFromArgb, Hct, SchemeTonalSpot, MaterialDynamicColors, DynamicColor } from '@material/material-color-utilities'

export type Mode = 'system' | 'light' | 'dark'
export const SEEDS = ['#6750A4', '#0061A4', '#006E1C', '#9C4146', '#8B5000', '#006A6A', '#7D5260', '#4A6363']

const kebab = (s: string) => s.replace(/[A-Z]/g, (m) => '-' + m.toLowerCase())

export function loadPrefs(): { seed: string; mode: Mode } {
  return {
    seed: localStorage.getItem('im.seed') || SEEDS[0],
    mode: (localStorage.getItem('im.mode') as Mode) || 'system',
  }
}

export function savePrefs(seed: string, mode: Mode) {
  localStorage.setItem('im.seed', seed)
  localStorage.setItem('im.mode', mode)
}

const media = window.matchMedia('(prefers-color-scheme: dark)')

export function isDark(mode: Mode) {
  return mode === 'dark' || (mode === 'system' && media.matches)
}

/** 由种子色生成 Material You 动态配色，写入 --md-sys-color-* 变量 */
export function applyTheme(seed: string, mode: Mode) {
  const dark = isDark(mode)
  const scheme = new SchemeTonalSpot(Hct.fromInt(argbFromHex(seed)), dark, 0)
  const root = document.documentElement
  for (const key of Object.getOwnPropertyNames(MaterialDynamicColors)) {
    const dc = (MaterialDynamicColors as any)[key]
    if (dc instanceof DynamicColor) {
      root.style.setProperty(`--md-sys-color-${kebab(key)}`, hexFromArgb(dc.getArgb(scheme)))
    }
  }
  root.style.colorScheme = dark ? 'dark' : 'light'
  document.querySelector('meta[name=theme-color]')?.setAttribute('content', hexFromArgb(MaterialDynamicColors.surface.getArgb(scheme)))
}

export function onSystemThemeChange(cb: () => void) {
  media.addEventListener('change', cb)
  return () => media.removeEventListener('change', cb)
}
