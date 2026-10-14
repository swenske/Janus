# Auto-scaling

A node the Controller created on a hypervisor can grow while it runs, and
come back to its size once the load is gone. More vCPUs and more memory,
plugged into the running virtual machine: no restart and no connection
lost. Auto-scaling reads the node's load, decides against your
thresholds, and writes every step in the machine's journal.

> [!NOTE]
> Start with **Observe**: it writes what it would do, and the size it
> would have given, without resizing anything. Run it on your real
> traffic, adjust the thresholds, then turn it **On**.

It's for the machines the Controller makes ([Hypervisors](../hypervisors.md)),
on libvirt/KVM and Proxmox VE. It needs a node image from 2026-10-14 or
later: its kernel takes memory plugged in while it runs.

## How a machine grows

A machine has its **size**: the vCPUs and memory it's created with, and
the size it comes back to. It also has the **maxima** its policy allows.
It's started with room for the maxima:

| | libvirt | Proxmox VE |
|---|---|---|
| vCPUs | `<vcpu current='N'>MAX</vcpu>`: plugged and unplugged with `setvcpus --live` | `cores` is the maximum, `vcpus` how many are plugged |
| Memory | one NUMA cell of its size, plus a virtio-mem device up to the maximum, nothing plugged at start; changes by 128 MiB steps | memory modules of 512 MiB above a static 1024 MiB (1 GiB modules past 32 of them); changes module by module |
| Needs | nothing more | the `hotplug` option with `cpu` and `memory`, and NUMA: the Controller sets them |

Setting a policy, changing its maxima or removing it restarts the
machine once: the node shuts down cleanly and starts again with room to
grow, or without it. The mode and the thresholds change without a
restart.

On Proxmox VE, a scalable machine's memory is at least 1024 MiB, then
1536, 2048 and so on. On libvirt, any even number of MiB.

On the node:

- a vCPU plugged in is brought online, and HAProxy reloads to run a
  thread per CPU - unless its configuration fixes `nbthread` or
  `cpu-map`;
- memory plugged in is onlined by the kernel where it can be taken back
  (`ZONE_MOVABLE`);
- the node's event log says what changed: `system.cpus_changed`
  (and whether HAProxy was reloaded), `system.memory_changed`, and
  `kernel.oom_kill` when the kernel kills a process for lack of memory.

## What it reads, and when it steps

Every 15 seconds, auto-scaling reads the node:

- HAProxy's idle time (`show info`'s `Idle_pct`);
- how busy the CPUs are, and how long they waited for the host (steal);
- the memory available;
- the OOM kills since boot;
- HAProxy's connections against its `maxconn`.

| Step | When (defaults) |
|---|---|
| **Memory up** | Less than 20 % of the memory available for 2 minutes, or at once after an OOM kill. A quarter more, in whole steps. |
| **A vCPU more** | HAProxy idle less than 30 % of the time on average over 5 minutes, or the CPUs more than 75 % busy. |
| **A vCPU less** | HAProxy would stay idle at least 50 % of the time with one fewer vCPU, and the CPUs at most 50 % busy, all through the last 30 minutes. |
| **Memory down** | At least 40 % of the memory would stay available with a fifth less, all through the last 30 minutes. Never below its size. |
| **Between steps** | 5 minutes at least between two steps up, an OOM kill aside. At most 6 steps an hour. One step at a time. |

How it judges and steps:

- **Projected load.** Every measurement is judged at the size the step
  would give. For example, 90 % busy over 2 vCPUs is 60 % over 3. The
  decision is about the result, not the trend.
- **Memory first on the way up.** Adding a vCPU makes HAProxy reload, and
  the old process stays alive with its connections for a while. A vCPU
  doesn't come while memory is short.
- **vCPUs first on the way back.**
- **Back towards its size.** It steps back once the load has stayed low
  for the whole of the return delay since the last step, one step at a
  time.

HAProxy's documentation gives no number for any of this. The defaults are
Janus's starting points:

- an `Idle_pct` close to 0 means HAProxy always has work waiting;
- the up and down thresholds are far apart, so that a machine that just
  grew doesn't shrink back at once: the Controller refuses a policy
  where they overlap.

What no step can fix is said, not acted on:

- HAProxy near its `maxconn`: a limit of its configuration, which more
  memory doesn't raise;
- threads fixed by its configuration: more vCPUs wouldn't be used;
- a host short of CPU: when the vCPUs wait for the host more than 10 %
  of the time (the kernel's steal time), another vCPU would wait as
  well. Steal time isn't counted as busy, and no vCPU is added while it
  lasts. A node before v2026.10.14-2 doesn't report it: for those, the
  CPUs' busy time includes it.

## Resizing

Its mode **On**, each step resizes the running machine:

1. **Memory added only if the host has room.** The host must keep a
   twentieth of its own memory available, 512 MiB at least - available:
   free, or held by its page cache, which the kernel gives back when
   asked (Proxmox VE's `available`; libvirt's free memory, buffers and
   cache). Otherwise the step is refused, written once in the journal
   while the refusal lasts, and tried again with the next decision.
2. **The hypervisor plugs in or takes out what the step says** - vCPUs,
   virtio-mem's requested size, Proxmox VE's memory modules.
3. **Auto-scaling waits until the node shows it**: its CPUs online, its
   memory moved by what was added or taken, within 90 seconds.
4. **The journal says what the machine really has after it.** Memory in
   use may not all come back at once: the step records what did.

A machine paused, or set to observe, goes back to its size at once.
A reboot keeps the size it was given. A restart through **Edit** starts
it at its size again.

## The kernel parameters

**Kernel parameters follow** (`"sysctl": true`) makes the node's kernel
parameters follow the machine. The node already suggests values on its
**System › Sysctl** page, from its memory and what it observed (see
[Kernel tuning](kernel-tuning.md#suggestions)), and memory plugged in
adds the `follow_memory` suggestions for `tcp_mem`, `file-max` and
`nf_conntrack_max`. Auto-scaling applies them the way you would:

- on trial, and confirmed over a connection of its own once the node and
  HAProxy still answer - otherwise the node takes them back by itself
  within 2 minutes;
- right after a memory step, and every 10 minutes;
- **never a parameter someone else confirmed**: the node's sysctl history
  says who changed what - auto-scaling is `auto-scaling` there;
- a parameter at most once every 6 hours - the `follow_memory` rules
  aside, which follow every memory step - so two rules can't take turns
  on it;
- back at its size, what followed the memory goes back to what it was,
  unless someone changed it since.

Observing, each change it would make is written in the journal, once.

## Observing

Observing changes nothing on the machine. Each step auto-scaling would
take is:

- written in the journal, with the measurement that called for it;
- simulated: the next decisions judge the load at the size it would have
  given.

So the journal shows how far it would have grown, and when it would
have come back. On the machine's card:

- **Size** says what it would be;
- the status line says what auto-scaling watches, or why it waits;
- **Journal** lists the steps, newest first.

## Setting it

**Edit** on the machine's card, in the **Hypervisors** tab:

- **Mode**:
  - **Observe**;
  - **On**;
  - **Paused** (back to its size, room kept: no restart to turn it on
    again);
  - **None** (its size is fixed - removing the policy restarts it).
- **Up to**: the maxima.
- **Thresholds**: what's above, each one changeable.
- **Kernel parameters follow**.

From the API, `PATCH /api/machines/{id}`:

```json
{"autoscale": {"mode": "on", "max_vcpus": 4, "max_memory_mib": 4096, "sysctl": true}}
```

The thresholds are optional, their defaults left out. Each one:

| Field | Default |
|---|---|
| `cpu_up_idle_percent` | 30 |
| `cpu_up_busy_percent` | 75 |
| `cpu_up_after_seconds` | 300 |
| `cpu_down_idle_percent` | 50 |
| `memory_up_available_percent` | 20 |
| `memory_up_after_seconds` | 120 |
| `memory_down_available_percent` | 40 |
| `return_after_seconds` | 1800 (60 at least) |
| `cooldown_seconds` | 300 |
| `max_steps_per_hour` | 6 |

`{"autoscale": null}` removes the policy. The same body works on
`POST /api/machines`, to create a machine with room to grow from the
start.

`GET /api/machines/{id}/autoscale` answers:

- the policy;
- what auto-scaling remembers: the size it judges, its last steps, its
  status and warnings;
- the journal (`?n=`, 200 entries by default, 1000 at most);
- the defaults.

A machine's record keeps its size as created. What it has now is `size`,
read from the hypervisor, with the most it can have without a restart. A
size auto-scaling didn't give it is noted in its history, once.

The modes, from the API:

| `mode` | What happens |
|---|---|
| `observe` | Steps journaled and simulated, nothing resized. |
| `on` | Steps taken. |
| `off` | Paused: back to its size, the room to grow kept. |
