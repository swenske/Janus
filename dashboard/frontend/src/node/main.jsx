import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import '../shared/theme.css'
import './node.css'
import App from './App.jsx'
import { ConfirmProvider, ToastProvider } from '../shared/ui.jsx'
import { SplashProvider } from '../shared/Splash.jsx'
import { MetricsProvider, NodeStatusProvider, RefreshProvider } from './hooks.jsx'

createRoot(document.getElementById('root')).render(
  <StrictMode>
    <ToastProvider>
      <ConfirmProvider>
        <RefreshProvider>
          <MetricsProvider>
            <NodeStatusProvider>
              {/* The node, the account's rights on it, its system, its first metrics (App.jsx). */}
              <SplashProvider steps={['node', 'me', 'overview', 'metrics']}>
                <App />
              </SplashProvider>
            </NodeStatusProvider>
          </MetricsProvider>
        </RefreshProvider>
      </ConfirmProvider>
    </ToastProvider>
  </StrictMode>,
)
