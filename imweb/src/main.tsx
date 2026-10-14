import { createRoot } from 'react-dom/client'
import './md'
import './styles.css'
import App from './App'
import { applyTheme, loadPrefs } from './theme'

const p = loadPrefs()
applyTheme(p.seed, p.mode) // 首帧前上色，避免闪烁
createRoot(document.getElementById('root')!).render(<App />)
