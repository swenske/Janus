import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import './shared/theme.css'
import './index.css'
import App from './App.jsx'
import { ConfirmProvider, ToastProvider } from './shared/ui.jsx'
import { SplashProvider } from './shared/Splash.jsx'

createRoot(document.getElementById('root')).render(
  <StrictMode>
    <ToastProvider>
      <ConfirmProvider>
        {/* The session, the fleet, then its nodes' status (App.jsx). */}
        <SplashProvider steps={['session', 'inventory', 'status']}>
          <App />
        </SplashProvider>
      </ConfirmProvider>
    </ToastProvider>
  </StrictMode>,
)
