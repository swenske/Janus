import { BookOpen, GitBranch, Hammer } from 'lucide-react'
import { useEffect, useState } from 'react'
import { Logo, ThemeToggle } from '@shared/theme.jsx'
import { REPO, DOCS } from './api.js'
import Builder from './Builder.jsx'
import Landing from './Landing.jsx'

// A two-page site: client-side routing with the History API (the Go
// server answers index.html for / and /builder, and with a 404 for any
// other path).
export function navigate(to) {
  window.history.pushState(null, '', to)
  window.dispatchEvent(new PopStateEvent('popstate'))
  window.scrollTo(0, 0)
}

export function Link({ to, className, children }) {
  return (
    <a
      href={to}
      className={className}
      onClick={(e) => {
        if (e.metaKey || e.ctrlKey || e.shiftKey || e.button !== 0) return
        e.preventDefault()
        navigate(to)
      }}
    >
      {children}
    </a>
  )
}

function usePath() {
  const [path, setPath] = useState(window.location.pathname)
  useEffect(() => {
    const on = () => setPath(window.location.pathname)
    window.addEventListener('popstate', on)
    return () => window.removeEventListener('popstate', on)
  }, [])
  return path
}

// What the server answers with a 404 for any other path (site/backend):
// one way back to each part of the site.
function NotFound({ path }) {
  return (
    <section className="hero">
      <h1>Page not found</h1>
      <p className="lead">
        There&apos;s nothing at <code>{path}</code>.
      </p>
      <div className="row hero-actions">
        <Link to="/" className="button">
          Home
        </Link>
        <a href="/docs/" className="button">
          <BookOpen size={15} /> Documentation
        </a>
        <Link to="/builder" className="button">
          <Hammer size={15} /> Image builder
        </Link>
      </div>
    </section>
  )
}

export default function App() {
  const path = usePath()
  const builder = path.startsWith('/builder')
  const home = path === '/'
  useEffect(() => {
    document.title = builder ? 'Image builder - Janus' : home ? 'Janus - an immutable Linux for HAProxy load balancers' : 'Page not found - Janus'
  }, [builder, home])
  return (
    <div className="site">
      <header className="topbar">
        <Link to="/" className="brand">
          <Logo size={30} />
          <span>Janus</span>
        </Link>
        <nav className="row">
          <Link to="/builder" className={builder ? 'nav active' : 'nav'} title="Image builder">
            <Hammer size={15} /> <span className="nav-label">Image builder</span>
          </Link>
          <a className="nav" href={`${DOCS}/`}>
            <BookOpen size={15} /> <span className="nav-label">Docs</span>
          </a>
          <a className="nav" href={REPO}>
            <GitBranch size={15} /> <span className="nav-label">GitHub</span>
          </a>
          <ThemeToggle />
        </nav>
      </header>
      <main>{builder ? <Builder /> : home ? <Landing /> : <NotFound path={path} />}</main>
      <footer className="footer">
        <div className="row">
          <Logo size={18} />
          <span>Janus - an immutable, API-driven Linux for HAProxy.</span>
        </div>
        <div className="row">
          <a href={REPO}>Source</a>
          <a href={`${REPO}/releases`}>Releases</a>
          <a href="https://hub.docker.com/r/swenske/janus-controller">Controller image</a>
          <a href={`${REPO}/issues`}>Issues</a>
        </div>
      </footer>
    </div>
  )
}
