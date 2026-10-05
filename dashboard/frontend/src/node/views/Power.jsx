import { Eraser, Power as PowerIcon, RefreshCcw, RotateCw } from 'lucide-react'
import { useState } from 'react'
import { postJSON } from '../api.js'
import { Card, PageHeader, useAction, useConfirm, useToast } from '../../shared/ui.jsx'
import { useNodeStatus } from '../hooks.jsx'
import { useMay } from '../may.js'
import { WaitForNode } from '../waitForNode.jsx'

const ACTIONS = [
  {
    id: 'restart',
    method: 'SystemService/Restart',
    icon: RefreshCcw,
    title: 'Restart janusd',
    text: "Restarts the node's control-plane daemon only. HAProxy keeps serving traffic throughout: the new janusd takes it over without a reload gap.",
    button: 'Restart janusd',
    confirm: { title: 'Restart janusd?', body: 'The API is unavailable for a few seconds. HAProxy is not interrupted.', action: 'Restart' },
    expectReboot: false,
  },
  {
    id: 'reboot',
    method: 'SystemService/Reboot',
    icon: RotateCw,
    title: 'Reboot',
    text: 'HAProxy is soft-stopped first (in-flight connections get 5 s), then the machine reboots into its active slot.',
    button: 'Reboot…',
    confirm: { title: 'Reboot this node?', body: 'It stops serving traffic until it is back up, usually well under a minute.', action: 'Reboot', danger: true },
    expectReboot: true,
  },
  {
    id: 'shutdown',
    method: 'SystemService/Shutdown',
    icon: PowerIcon,
    title: 'Shut down',
    text: 'Same graceful stop, then the machine powers off. Bringing it back needs your hypervisor or physical access - not this page.',
    button: 'Shut down…',
    confirm: { title: 'Power this node off?', body: "Nothing here can turn it back on: you'll need the hypervisor or physical access.", action: 'Power off', danger: true, typeToConfirm: 'shutdown' },
    expectReboot: false,
  },
  {
    id: 'reset',
    method: 'SystemService/Reset',
    icon: Eraser,
    title: 'Reset',
    text: 'Wipes the persistent state - PKI, applied HAProxy config, Controller registration - and reboots. The node generates a new CA and admin certificate, printed once on its console: the Controller and every current certificate lose access to it.',
    button: 'Reset…',
    confirm: {
      title: 'Reset this node to its just-installed state?',
      body: "Its PKI is regenerated: this Controller loses access and you'll have to add the node again with the new admin certificate from its console. Applied HAProxy config is lost.",
      action: 'Wipe and reboot',
      danger: true,
    },
    expectReboot: true,
  },
]

export default function Power() {
  const { overview } = useNodeStatus()
  const [waiting, setWaiting] = useState(null)
  const [resetDone, setResetDone] = useState(false)
  const [busy, run] = useAction()
  const confirm = useConfirm()
  const toast = useToast()
  const may = useMay()
  const hostname = overview.data?.hostname

  const act = async (a) => {
    const ok = await confirm({ ...a.confirm, body: <p>{a.confirm.body}</p>, typeToConfirm: a.id === 'reset' ? hostname || 'reset' : a.confirm.typeToConfirm })
    if (!ok) return
    const res = await run(() => postJSON('/api/system/power', { action: a.id, wipe_state: a.id === 'reset' }), `${a.title}: requested`)
    if (res === undefined) return
    if (a.id === 'shutdown') return
    if (a.id === 'reset') {
      setResetDone(true)
      return
    }
    setWaiting({ action: a, boot: overview.data?.system?.boot_time_unix })
  }

  return (
    <>
      <PageHeader title="Power" subtitle="Restart, reboot, shut down or reset this node" />
      {resetDone && (
        <div className="notice warn" style={{ marginBottom: '1rem' }}>
          The node is resetting. Its new CA and admin certificate will be printed once on its console: remove it from the Controller's node list and add it again with that
          certificate (or let it self-register, if it was installed with a Controller address).
        </div>
      )}
      {waiting && (
        <div style={{ marginBottom: '1rem' }}>
          <WaitForNode
            expectDown
            previousBoot={waiting.action.expectReboot ? waiting.boot : undefined}
            label={`${waiting.action.title} in progress`}
            onBack={() => {
              toast(`${waiting.action.title}: the node is back`)
              setWaiting(null)
            }}
          />
        </div>
      )}
      <div className="grid grid-2">
        {ACTIONS.filter((a) => may(a.method)).map((a) => (
          <Card key={a.id} title={a.title} icon={a.icon}>
            <p className="muted" style={{ marginTop: 0 }}>
              {a.text}
            </p>
            <button className={a.id === 'restart' ? '' : 'danger'} disabled={busy || !!waiting || resetDone} onClick={() => act(a)}>
              <a.icon size={15} /> {a.button}
            </button>
          </Card>
        ))}
      </div>
    </>
  )
}
