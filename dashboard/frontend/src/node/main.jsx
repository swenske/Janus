import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import '../shared/theme.css'
import '../shared/palettes.css'
import '../shared/controller.css'
import '../shared/appearance.js'
import './node.css'
import App from './App.jsx'
import { ConfirmProvider, ToastProvider } from '../shared/ui.jsx'
import { SplashProvider } from '../shared/Splash.jsx'
import { MetricsProvider, NodeStatusProvider, RefreshProvider } from './hooks.jsx'
import { accountPrefs, loadDisplay } from './display.js'
import { ReadOnlyProvider } from '../shared/readOnly.jsx'

// The account's time zone and rate unit first: every figure drawn follows them.
loadDisplay().finally(() => createRoot(document.getElementById('root')).render(
  <StrictMode>
    <ToastProvider>
      <ConfirmProvider>
        <RefreshProvider>
          <MetricsProvider>
            <NodeStatusProvider>
              {/* The node, the account's rights on it, its system, its first metrics (App.jsx). */}
              <SplashProvider steps={['node', 'me', 'overview', 'metrics']}>
                {/* The Read-only switch (#527), from the account's preferences. */}
                <ReadOnlyProvider prefs={accountPrefs} loaded>
                  <App />
                </ReadOnlyProvider>
              </SplashProvider>
            </NodeStatusProvider>
          </MetricsProvider>
        </RefreshProvider>
      </ConfirmProvider>
    </ToastProvider>
  </StrictMode>,
))
