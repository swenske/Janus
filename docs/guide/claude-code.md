# Claude Code and janusctl

Janus's skill for [Claude Code](https://claude.com/claude-code) teaches
it janusctl: ask in your own words - why a site answers 503, what
changed on a node this week, a server to add to a backend - and Claude
reads your nodes, proposes the change and makes it the way these pages
do: configurations validated on the node, trials that revert by
themselves, one node at a time, and nothing that reboots, moves traffic
or touches the nodes' trust unless you asked for exactly that.

## What it needs

- [janusctl](../janusctl.md), installed and signed in on the machine
  Claude Code runs on: Claude uses your contexts and never signs in
  itself - a sign-in is a browser page, a device code or an SSH key's
  passphrase, all yours.
- Claude Code.

## Installing the skill

### As a plugin

This repository is a Claude Code plugin marketplace, `janus`, offering
the `janusctl` plugin. In Claude Code:

```text
/plugin marketplace add swenske/Janus
/plugin install janusctl@janus
```

Or from the shell:

```sh
claude plugin marketplace add swenske/Janus
claude plugin install janusctl@janus
```

Claude Code 2.1.275 or later does both in one step:
`/plugin install janusctl --marketplace swenske/Janus`.

The next session has it (`/reload-plugins` in the current one). Claude
uses it by itself when you speak of Janus nodes, janusctl, a fleet or a
cluster; `/janusctl:janusctl` calls it explicitly. `--scope project`
records the plugin in a repository's `.claude/settings.json` instead, for
everyone working in it - the repository of your `haproxy.cfg` files,
say.

### By hand

The skill is one folder, `claude-code/janusctl/skills/janusctl/` in this
repository. Copy it into `~/.claude/skills/` for every project, or into a
repository's `.claude/skills/` for whoever works in that one:

```sh
git clone --depth 1 https://github.com/swenske/Janus janus-src
mkdir -p ~/.claude/skills
cp -r janus-src/claude-code/janusctl/skills/janusctl ~/.claude/skills/
```

It is then `/janusctl`. A copy doesn't update itself: copy it again
after a release.

## Using it

```text
edge-1 answers 503 for shop.example.com - what's wrong?
What changed in edge-1's haproxy.cfg this week?
Add 10.0.0.7:8080 as app3 to the app backend on edge-1.
Drain app2 on edge-1 while I patch it, then put it back.
Which nodes have a certificate expiring within 30 days?
Upgrade edge-2 to the latest release.
```

Claude names the nodes with `-n` and reads what answers the question:
`janusctl -n edge-1 tui -once` for a first look, `system logs`,
`haproxy backends`, `haproxy errors`, `system probe` to test the
network from the node itself. A change - a configuration, a map entry,
a firewall ruleset - it shows first (the diff), applies, and checks it
took: `haproxy apply-config` is validated on the node before anything
changes, `network apply` and `network firewall apply` are trials that
revert unless janusctl confirms them over a new connection. What
reboots, powers off, moves traffic or touches the nodes' trust, it runs
only when you asked for that very action on those very nodes, after
telling you what will happen.

Each command is marked in the skill with what it does to the node:

| | Commands | Claude runs them |
|---|---|---|
| reads | `system info`, `system logs`, `haproxy backends`, `haproxy get-config`, `network status`... | when they help answer |
| changes | `haproxy apply-config`, `haproxy server-state`, `haproxy map-set`, `network apply`, `system sysctl set`, `cluster apply`... | when you asked for that change - shown first, checked after |
| critical | `system reboot`, `system reset`, `lifecycle upgrade`, `cluster drain`, `access trust-reset`, `fleet adopt`... | when you asked for exactly that, after a yes |

The full list is the skill's
[references/commands.md](../../claude-code/janusctl/skills/janusctl/references/commands.md).

## What Claude may do

Claude Code asks before each janusctl command you haven't allowed, and
the skill tells Claude what to leave to you. What a node lets Claude do
is its certificate's role: every node checks it on every call and logs
who acted - as long as Claude can reach no other certificate.

### The role it needs

Give Claude a context of its own, with no more than it needs - a reader
to investigate, an operator to run HAProxy:

1. On the Controller, your account's dialog, **SSH keys for janusctl**:
   add a key with the lower role (an Ed25519 key in ssh-agent - janusctl
   renews its certificate by itself, without asking anything).
2. Sign that context in, in a configuration of its own:

   ```sh
   JANUSCONFIG=~/.config/janus-claude/janusctl.json \
     janusctl login -controller janus-controller.example.com \
     -user sam -ssh-key ~/.ssh/id_ed25519_claude.pub
   ```

3. Have Claude Code's commands use it - `env` in `~/.claude/settings.json`,
   or in a repository's `.claude/settings.json`, an absolute path:

   ```json
   {
     "env": {
       "JANUSCONFIG": "/home/sam/.config/janus-claude/janusctl.json"
     }
   }
   ```

Claude's janusctl commands then use that context - `janusctl context list`
shows it alone - and each node refuses what its role doesn't allow. A
certificate approved on the Controller's page can carry a lower role
too ([janusctl on the Controller](../../dashboard/README.md#janusctl)).

### Your other credentials out of reach

A configuration of its own isn't a lock: your other contexts are still
on the disk, readable by anything that runs as you - Claude's commands
too: `JANUSCONFIG=~/.config/janus/janusctl.json janusctl ...` would use
them. Claude Code asks you before such a command - an allow rule doesn't
match past the assignment of a variable it doesn't know, and
`~/.config/janus` is outside the working directory - and the skill tells
Claude never to work around a refusal. Those are safeguards. For a
boundary, run Claude Code where your other credentials aren't:

- **An account of its own** on the machine, holding the context with the
  lower role only - signed in from that account, its SSH key in that
  account's ssh-agent - and Claude Code started as that account.
  `JANUSCONFIG` isn't needed there.
- **A container or a virtual machine** with Claude Code, janusctl and that
  configuration only - a dev container, say.

Claude Code's own [sandbox](https://code.claude.com/docs/en/sandboxing.md)
can hide a directory from its commands, but janusctl doesn't work in it
yet: the sandbox lets commands out through its proxy only, and
janusctl's connections to the Controller - the certificate's renewal,
the list of nodes - don't use it.

### The critical commands, always asked

These [settings](https://code.claude.com/docs/en/permissions.md) make
Claude Code ask before every critical command, whatever was allowed
before - in auto mode too. The `*` after `janusctl` stands for the
global flags (`-n edge-1`). Add them to `~/.claude/settings.json` or a
repository's `.claude/settings.json`:

```json
{
  "permissions": {
    "ask": [
      "Bash(janusctl *system service *)",
      "Bash(janusctl *system state export *)",
      "Bash(janusctl *system state import*)",
      "Bash(janusctl *system reboot*)",
      "Bash(janusctl *system shutdown*)",
      "Bash(janusctl *system reset*)",
      "Bash(janusctl *access trust-set *)",
      "Bash(janusctl *access trust-reset*)",
      "Bash(janusctl *access rotate-ca *)",
      "Bash(janusctl *cluster create *)",
      "Bash(janusctl *cluster remove *)",
      "Bash(janusctl *cluster replace *)",
      "Bash(janusctl *cluster delete *)",
      "Bash(janusctl *cluster set *)",
      "Bash(janusctl *cluster leave*)",
      "Bash(janusctl *cluster drain*)",
      "Bash(janusctl *cluster undrain*)",
      "Bash(janusctl *pki generate-client-config *)",
      "Bash(janusctl *fleet init *)",
      "Bash(janusctl *fleet recover*)",
      "Bash(janusctl *fleet adopt *)",
      "Bash(janusctl *fleet issuer sign *)",
      "Bash(janusctl *fleet issuer accept *)",
      "Bash(janusctl *fleet issuer revoke *)",
      "Bash(janusctl *lifecycle install *)",
      "Bash(janusctl *lifecycle upgrade *)",
      "Bash(janusctl *lifecycle rollback*)"
    ]
  }
}
```

A rule matches the command as written: it's a safety net. The boundary
is the role, with your other credentials out of reach.

## Keeping it current

The plugin follows this repository's `main`: `claude plugin update
janusctl@janus` (or **Update** in `/plugin`) takes the newest - Claude
Code doesn't update a marketplace other than Anthropic's by itself
unless you turn its auto-update on in `/plugin`. A command newer than
your janusctl may be in the skill: it has Claude check `janusctl help`
before a flag it hasn't seen, and the installed janusctl wins. To follow
a release rather than `main`, add the marketplace at its tag -
`/plugin marketplace add swenske/Janus#vYYYY.MM.DD` - and again at the
next one.

## How the skill is made

The plugin is `claude-code/janusctl/` (`.claude-plugin/plugin.json`,
`skills/janusctl/`); the marketplace that offers it is
`.claude-plugin/marketplace.json` at the repository's root.
`SKILL.md` is written by hand; `references/commands.md` is generated
from janusctl's command tree and `cmd/janusctl/effects.go` - what each
command does to a node - with `go test ./cmd/janusctl -run
TestSkillCommands -update`. A new command without an entry in
`effects.go` fails `go test`, every janusctl command line `SKILL.md` and
this page show must be one janusctl has, flags included, and the ask
rules above are `effects.go`'s critical commands. `claude plugin
validate .` checks the manifests - its one warning, no `version`, is on
purpose: without one, an update is a new commit of the plugin's folder.
