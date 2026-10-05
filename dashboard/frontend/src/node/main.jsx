import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import '../shared/theme.css'
import './node.css'
import App from './App.jsx'
import { ConfirmProvider, ToastProvider } from '../shared/ui.jsx'
import { MetricsProvider, NodeStatusProvider, RefreshProvider } from './hooks.jsx'

createRoot(document.getElementById('root')).render(
  <StrictMode>
    <ToastProvider>
      <ConfirmProvider>
        <RefreshProvider>
          <MetricsProvider>
            <NodeStatusProvider>
              <App />
            </NodeStatusProvider>
          </MetricsProvider>
        </RefreshProvider>
      </ConfirmProvider>
    </ToastProvider>
  </StrictMode>,
)
