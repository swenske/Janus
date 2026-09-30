import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import './shared/theme.css'
import './index.css'
import App from './App.jsx'
import { ConfirmProvider, ToastProvider } from './shared/ui.jsx'

createRoot(document.getElementById('root')).render(
  <StrictMode>
    <ToastProvider>
      <ConfirmProvider>
        <App />
      </ConfirmProvider>
    </ToastProvider>
  </StrictMode>,
)
