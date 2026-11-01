import { installMainGenerationTransport } from './services/generation/generation-runtime'
import { mainGenerationTransport } from './services/generation/main-generation-transport'
import { ipc } from './services/ipc-client'
import { bootstrapBrowserTransport, getBrowserTransport } from './services/browser-transport'
import React from 'react'
import ReactDOM from 'react-dom/client'
import RendererStartup from './components/startup/RendererStartup'
import BrowserDialogs from './components/BrowserDialogs'
// 纸墨样式只在入口引入：原语组件不带样式副作用，Node 侧打包（如真实供应商资格脚本）才能引用它们。
import './styles/paper-ink/index.css'
import './index.css'
// 外壳样式同样只在入口引入，排在全局样式之后，颜色桥接才能覆盖 index.css 里的 --color-*。
import './components/stage/stage.css'

// Bootstrap early, but let RendererStartup own visible retry/error handling through its reads.
// The first operation waits for both the cookie/CSRF session and its tab-scoped event stream.
if (ipc.runtimeKind === 'web') {
  void bootstrapBrowserTransport().catch(() => undefined)
  window.addEventListener('pagehide', () => getBrowserTransport().depart())
  // Back/forward cache can resurrect a page whose old project lease was released.
  // Reload its read model rather than operating with stale project authority.
  window.addEventListener('pageshow', event => { if (event.persisted) window.location.reload() })
}

installMainGenerationTransport(mainGenerationTransport)

ReactDOM.createRoot(document.getElementById('root')!).render(
  <React.StrictMode>
    <RendererStartup />
    {ipc.runtimeKind === 'web' && <BrowserDialogs />}
  </React.StrictMode>,
)
