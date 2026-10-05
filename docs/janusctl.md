# janusctl

`janusctl` is Janus's command-line client: every node's API from your
terminal, signed in to a Controller - or keeping a fleet itself, without
one. Every command, its arguments and flags: the [janusctl
reference](guide/janusctl-reference.md).

## Installing janusctl

`janusctl`, the command-line client for a node's API, is published as a
Debian package with every [release](https://github.com/swenske/Janus/releases),
for amd64 and arm64. It is a static binary: the same package installs on
any Debian or Ubuntu release (apt 2.4 or later for the `.asc` key below -
Debian 12, Ubuntu 22.04 and newer).

```sh
# 1. The repository's signing key
sudo install -d -m 0755 /etc/apt/keyrings
sudo curl -fsSL https://apt.sw-servers.net/apt-sw-servers.net.gpg.asc \
  -o /etc/apt/keyrings/apt-sw-servers.net.asc

# 2. The repository
echo "deb [signed-by=/etc/apt/keyrings/apt-sw-servers.net.asc] https://apt.sw-servers.net/janus stable main" \
  | sudo tee /etc/apt/sources.list.d/janus.list

# 3. Install
sudo apt-get update
sudo apt-get install janusctl
janusctl version
```

The key's fingerprint is `0731 333D 9DDF FF94 08CD 6AEC A333 9293 BD0C BBDC`
(`gpg --show-keys /etc/apt/keyrings/apt-sw-servers.net.asc`). Later
releases come with `apt-get upgrade`.

Without the repository, each release also carries
`janusctl_<version>_<amd64|arm64>.deb` (`sudo apt install
./janusctl_<version>_amd64.deb`); `make build` builds it from source.

### Shell completion

The package installs completion for bash, zsh and fish: commands and
their flags, your contexts and nodes, and what's on the node itself -
services, maps and their keys, certificates, HAProxy files, interfaces,
firewall sets, paths (`system cat /etc/hap<Tab>`) - asked of it in a
couple of seconds at most, never prompting. zsh and fish show what each
candidate is (with [fzf-tab](https://github.com/Aloxaf/fzf-tab), zsh's
menu becomes a fuzzy finder). Without the package:

```sh
source <(janusctl completion bash)                 # ~/.bashrc
source <(janusctl completion zsh)                  # ~/.zshrc, after compinit
janusctl completion fish > ~/.config/fish/completions/janusctl.fish
```

`janusctl help [COMMAND]` lists the commands, or one's flags.

### In a terminal

What's left out is asked instead of refused, in a list filtered as you
type (↑↓, Enter; Tab marks several): `janusctl` alone picks a command
(`janusctl system` one of its commands), a context with several nodes
and no `-n` picks them (`-n '?'` too), and an argument janusctl can list
is picked - `system logs` the service, `haproxy map-get` the map,
`system cat` a file, browsing the node's directories. The command line
that would have done it is shown after, to type next time. Output gets
colours and symbols there too - never in a pipe, a script or with
`NO_COLOR`; `JANUS_NO_PICKER=1` keeps the usage errors instead of the
lists.

## Using janusctl

Sign in to your Janus Controller once; janusctl then reaches its nodes
directly, with a certificate of the Controller's fleet for your account:

```sh
# With an SSH key of your account - added on the Controller's page (your
# account, "SSH keys for janusctl"), which also shows this command:
janusctl login -controller janus-controller.example.com \
  -controller-fingerprint DC:CC:FF:... \  # the Controller's certificate, checked
  -user sam -ssh-key ~/.ssh/id_ed25519.pub
janusctl nodes                            # the nodes, and which trust the fleet
janusctl -n edge-1 system info            # one node
janusctl -n edge-1,edge-2 haproxy show-info
janusctl -all version                     # every node of the fleet
janusctl context list                     # the Controllers signed in to
```

The certificate is for the SSH key itself and lasts 12 hours: with the
key in ssh-agent, janusctl renews it by itself. `-ssh-key` takes the key
in ssh-agent (its `.pub`; Ed25519 - the agent can't sign a TLS handshake
with ECDSA or RSA) or a private key file (Ed25519, ECDSA or RSA; its
passphrase asked); a FIDO key (`sk-...`) can't sign janusctl's
connections. Each key can carry a lower role than your account's.

Without an SSH key: `janusctl login -controller ...` alone opens the
Controller's page in the browser - sign in there (second factor and
all), compare the key it shows with the one janusctl printed, approve;
`-device` instead shows a code to enter on the page from any machine (a
server without a browser). Either way the certificate is for a key
janusctl made and lasts 12 hours; then `janusctl login` again.

In CI, an API token of the account instead (`JANUS_TOKEN=janus_...
janusctl login -controller ...`): a certificate for an hour, with the
token's role, renewed while `JANUS_TOKEN` is set.

An account with grants on labelled nodes (or a token narrowed to some)
gets a scoped certificate: what it may do on each node it reaches -
`janusctl login` says it, e.g. `scoped: os:operator (haproxy) on 3
nodes` -, checked by each node itself; `janusctl nodes` lists those
nodes only. A node labelled after the sign-in needs a new one. Scoped
certificates open nodes of this release on; an older node refuses them.

The first login asks for the Controller's certificate: `-controller-
fingerprint` (the account dialog's command has it; on its host:
`openssl x509 -in <data-dir>/dashboard-identity.crt -noout -fingerprint
-sha256`) or `-controller-ca FILE` - or nothing, when the Controller's
page has a certificate of its own that this machine trusts. The SSH key signs a challenge for
the certificate janusctl saw: a signature relayed by another server is
refused.
Each node checks the role itself and logs who acted. The configuration
is `~/.config/janus/janusctl.json` (`JANUSCONFIG` elsewhere), each
context's key and certificate next to it.

A node's own certificate still works, and comes first: `janusctl
-endpoint NODE:9505 -ca ca.crt -cert admin.crt -key admin.key ...` - the
first-boot admin certificate, the way in when nothing else does.

### Without a Controller

janusctl can keep a fleet itself - its root's key in a recovery kit,
offline, each machine signing itself 12-hour certificates with an
issuing CA of its own:

```sh
janusctl -context lab fleet init -issuer alice-laptop janus-kit.age
janusctl fleet export provision/        # for image seed-fleet, install, NoCloud
janusctl fleet adopt edge-1 -endpoint 192.0.2.10 -ca-fingerprint 9bc3...   # its console's "ca sha256"
janusctl -all haproxy show-info
```

More machines, revoking one, recovering from the kit (a lost
Controller's too): [fleet-without-controller.md](fleet-without-controller.md).
