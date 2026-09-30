import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import '../shared/theme.css'
import './node.css'
import App from './App.jsx'
import { ConfirmProvider, ToastProvider } from '../shared/ui.jsx'
import { MetricsProvider, RefreshProvider } from './hooks.jsx'

createRoot(document.getElementById('root')).render(
  <StrictMode>
    <ToastProvider>
      <ConfirmProvider>
        <RefreshProvider>
          <MetricsProvider>
            <App />
          </MetricsProvider>
        </RefreshProvider>
      </ConfirmProvider>
    </ToastProvider>
  </StrictMode>,
)
