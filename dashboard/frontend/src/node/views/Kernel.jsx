import { Download, Terminal } from 'lucide-react'
import { download } from '../api.js'
import LogViewer from '../components/LogViewer.jsx'
import { Card, PageHeader, useAction } from '../../shared/ui.jsx'

export default function Kernel() {
  const [busy, run] = useAction()
  return (
    <>
      <PageHeader
        title="Kernel log"
        subtitle="The kernel ring buffer (dmesg), followed live"
        actions={
          <button disabled={busy} onClick={() => run(() => download('/api/dmesg?download=true', 'dmesg.txt'))}>
            <Download size={15} /> Download full dmesg
          </button>
        }
      />
      <Card title="dmesg" icon={Terminal}>
        <LogViewer url="/api/stream/dmesg" name="dmesg" emptyText="Reading the kernel ring buffer…" />
      </Card>
    </>
  )
}
