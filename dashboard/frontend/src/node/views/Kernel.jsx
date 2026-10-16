import { Download, Terminal } from 'lucide-react'
import { useState } from 'react'
import { download } from '../api.js'
import BootLog from '../components/BootLog.jsx'
import BootSelect from '../components/BootSelect.jsx'
import LogViewer from '../components/LogViewer.jsx'
import { Card, PageHeader, useAction } from '../../shared/ui.jsx'

export default function Kernel() {
  const [busy, run] = useAction()
  const [boot, setBoot] = useState(0)
  return (
    <>
      <PageHeader
        title="Kernel log"
        subtitle="The kernel ring buffer (dmesg), followed live - and previous boots', from the LOGS partition"
        actions={
          <>
            <BootSelect value={boot} onChange={setBoot} />
            <button disabled={busy} onClick={() => run(() => download(`/api/dmesg?download=true&boot=${boot}`, 'dmesg.txt'))}>
              <Download size={15} /> Download full dmesg
            </button>
          </>
        }
      />
      <Card title="dmesg" icon={Terminal}>
        {boot === 0 ? (
          <LogViewer url="/api/stream/dmesg" name="dmesg" emptyText="Reading the kernel ring buffer…" />
        ) : (
          <BootLog key={boot} url={`/api/dmesg?boot=${boot}`} name={`dmesg-boot${boot}`} />
        )}
      </Card>
    </>
  )
}
