import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import '@shared/theme.css'
import './site.css'
import App from './App.jsx'
import { ToastProvider } from '@shared/ui.jsx'

createRoot(document.getElementById('root')).render(
  <StrictMode>
    <ToastProvider>
      <App />
    </ToastProvider>
  </StrictMode>,
)
