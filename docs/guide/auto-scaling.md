# Auto-scaling

A node the Controller created on a hypervisor can grow while it runs, and
come back to its size once the load is gone. More vCPUs and more memory,
plugged into the running virtual machine: no restart and no connection
lost. Auto-scaling reads the node's load, decides against your
thresholds, and writes every step in the machine's journal.

> [!NOTE]
> This version **observes**: it writes what it would do, and the size it
> would have given, without resizing anything. Run it on your real
> traffic before letting it resize for real. That comes in a later
> version.

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
- how busy the CPUs are;
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
- threads fixed by its configuration: more vCPUs wouldn't be used.

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
  - **Paused** (back to its size, room kept: no restart to turn it on
    again);
  - **None** (its size is fixed - removing the policy restarts it).
- **Up to**: the maxima.
- **Thresholds**: what's above, each one changeable.

From the API, `PATCH /api/machines/{id}`:

```json
{"autoscale": {"mode": "observe", "max_vcpus": 4, "max_memory_mib": 4096}}
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
| `return_after_seconds` | 1800 |
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
