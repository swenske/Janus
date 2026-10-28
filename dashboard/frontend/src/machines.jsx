// The machines' badges, buttons and actions, shared by the Hypervisors
// page and the node cards (a node that is a Controller machine shows its
// VM's power and phase): in their own module so the nodes page, the
// first one painted, doesn't carry the Hypervisors page with them.
import { Code, Cpu, Loader2, Lock, LockOpen, Play, PowerOff, RotateCcw, SquareTerminal, Wrench } from 'lucide-react'
import { call, postJSON } from './call.js'
import { nodeCan, useCan, useMe } from './me.jsx'
import { Badge, stateTone, useAction, useConfirm } from './shared/ui.jsx'

const PHASE_LABEL = {
  pending: 'starting',
  'preparing-image': 'preparing the image',
  creating: 'creating',
  'waiting-registration': 'booting',
  ready: 'ready',
  updating: 'updating',
  failed: 'failed',
  destroying: 'destroying',
}

export function phaseTone(phase) {
  if (phase === 'ready') return 'ok'
  if (phase === 'failed') return 'danger'
  if (phase === 'destroying') return 'warn'
  return 'info'
}

export function PhaseBadge({ phase }) {
  const busy = !['ready', 'failed'].includes(phase)
  return (
    <Badge tone={phaseTone(phase)}>
      {busy && <Loader2 size={11} className="spin" />} {PHASE_LABEL[phase] || phase}
    </Badge>
  )
}

export function PowerBadge({ power }) {
  if (!power) return null
  return (
    <Badge tone={stateTone(power === 'off' ? 'stopped' : power)} dot>
      VM {power}
    </Badge>
  )
}

// useMachineActions: power, console, retry and destroy - shared by the
// machine cards and the node cards of machines.
export function useMachineActions({ onChanged, onConsole }) {
  const confirm = useConfirm()
  const [busy, run] = useAction()
  const power = async (m, action) => {
    const what = {
      reset: ['Reset', 'The virtual machine restarts at once, like pressing its reset button: whatever it was serving is cut off until it is back.'],
      'force-off': ['Force off', 'The virtual machine stops at once, like pulling its plug. For a clean stop, use Power on the node’s own page.'],
    }[action]
    if (what) {
      const ok = await confirm({ title: `${what[0]} ${m.spec.name}?`, body: <p>{what[1]}</p>, action: what[0], danger: true })
      if (!ok) return
    }
    await run(() => postJSON(`/api/machines/${m.id}/power`, { action }), `${m.spec.name}: ${action}`)
    onChanged()
  }
  const destroy = async (m) => {
    const ok = await confirm({
      title: `Destroy ${m.spec.name}?`,
      body: (
        <p>
          {m.node_id ? 'The node is shut down cleanly, then its' : 'Its'} virtual machine and disks are deleted from {m.hypervisor_name || 'its hypervisor'}, and the Controller
          forgets the node. This can&apos;t be undone.
        </p>
      ),
      action: 'Destroy',
      danger: true,
      typeToConfirm: m.spec.name,
    })
    if (!ok) return
    await run(() => call(`/api/machines/${m.id}`, { method: 'DELETE' }), `Destroying ${m.spec.name}`)
    onChanged()
  }
  const retry = async (m) => {
    await run(() => postJSON(`/api/machines/${m.id}/retry`), `Retrying ${m.spec.name}`)
    onChanged()
  }
  const release = async (m) => {
    const by = m.spec.managed_by || 'code'
    const ok = await confirm({
      title: `Release ${m.spec.name} from ${by}?`,
      body: (
        <p>
          Its pages may change it again - but {by} still manages it: its next run undoes what was changed here
          {by === 'terraform' ? ', and locks it again while its configuration says lock_ui = true' : ''}.
        </p>
      ),
      action: 'Release',
      danger: true,
    })
    if (!ok) return
    await run(() => postJSON(`/api/machines/${m.id}`, { locked: false }, 'PATCH'), `${m.spec.name} released`)
    onChanged()
  }
  const applySettings = async (m) => {
    const ok = await confirm({
      title: `Apply ${m.spec.name}’s hypervisor settings?`,
      body: (
        <>
          <p>
            The node shuts down cleanly, its virtual machine gets its settings - {driftList(m.hardware_drift)} - then starts again: whatever it serves is cut off until it is
            back.
          </p>
          <p>A cluster member is refused while it is in service: drain it on its cluster’s page first, and undrain it once it is back.</p>
        </>
      ),
      action: 'Apply and restart',
      danger: true,
    })
    if (!ok) return
    await run(() => postJSON(`/api/machines/${m.id}/apply-settings`), `${m.spec.name}: applying its settings`)
    onChanged()
  }
  return { busy, power, destroy, retry, release, applySettings, console: onConsole }
}

const SETTING_LABEL = { cpu: 'CPU model', 'guest-agent': 'QEMU guest agent' }

// driftList says a machine's hardware_drift in words.
function driftList(drift) {
  return (drift || []).map((d) => `${SETTING_LABEL[d.setting] || d.setting} ${d.have} → ${d.want}`).join(', ')
}

// DriftNotice: the machine's hypervisor settings that aren't the ones the
// Controller gives machines today - reported by its sync, applied only
// when an admin asks (a cold restart).
export function DriftNotice({ m, actions }) {
  const can = useCan()
  if (!m.hardware_drift?.length) return null
  return (
    <div className="notice warn small lock-notice drift-notice">
      <Cpu size={14} />
      <span className="grow">
        Its hypervisor settings aren’t the ones the Controller gives machines today:{' '}
        {m.hardware_drift.map((d, i) => (
          <span key={d.setting}>
            {i > 0 && ' · '}
            {SETTING_LABEL[d.setting] || d.setting} <span className="mono">{d.have}</span> → <span className="mono">{d.want}</span>
          </span>
        ))}
        . Applying them restarts it.
      </span>
      {can('admin') && m.phase === 'ready' && m.node_id && (
        <button className="small" onClick={() => actions.applySettings(m)} disabled={actions.busy} title="Shut the node down cleanly, apply the settings, start it again">
          <Wrench size={14} /> Apply settings
        </button>
      )}
    </div>
  )
}

// ManagedBadge: what manages the machine as code, and its lock.
export function ManagedBadge({ m }) {
  if (!m?.spec?.managed_by) return null
  return (
    <Badge tone={m.spec.locked ? 'info' : 'warn'}>
      {m.spec.locked ? <Lock size={11} /> : <Code size={11} />} {m.spec.managed_by}
    </Badge>
  )
}

// PowerButtons: an operator over everything's, or over the machines
// domain of the node it became (labels).
export function PowerButtons({ machine, vm, actions, labels }) {
  const me = useMe()
  if (!machine.vm_uuid || machine.phase === 'destroying' || !nodeCan(me, labels, 'operator', 'machines')) return null
  const off = vm?.power === 'off'
  return (
    <>
      {off ? (
        <button className="small" onClick={() => actions.power(machine, 'start')} disabled={actions.busy} title="Start the virtual machine">
          <Play size={14} /> Start
        </button>
      ) : (
        <>
          <button className="small ghost" onClick={() => actions.power(machine, 'reset')} disabled={actions.busy} title="Reset the virtual machine (hypervisor)">
            <RotateCcw size={14} />
          </button>
          <button className="small ghost" onClick={() => actions.power(machine, 'force-off')} disabled={actions.busy} title="Force the virtual machine off (hypervisor)">
            <PowerOff size={14} />
          </button>
        </>
      )}
      <button className="small ghost" onClick={() => actions.console(machine)} title="Serial console">
        <SquareTerminal size={14} />
      </button>
    </>
  )
}

// LockNotice says a machine is managed as code, and whether its pages may
// change it.
export function LockNotice({ m, actions }) {
  const can = useCan()
  if (!m.spec.managed_by) return null
  const by = m.spec.managed_by
  return m.spec.locked ? (
    <div className="notice small lock-notice">
      <Lock size={14} />
      <span className="grow">
        Managed by <strong>{by}</strong> and locked: change it there. A change made on this Controller would be undone by its next run.
      </span>
      {can('admin') && (
        <button className="small ghost" onClick={() => actions.release(m)} disabled={actions.busy} title="Let its pages change it again">
          <LockOpen size={14} /> Release
        </button>
      )}
    </div>
  ) : (
    <div className="notice warn small">
      Managed by <strong>{by}</strong>, released: a change made here is undone by its next run.
    </div>
  )
}
