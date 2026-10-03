package proxmox

import (
	"strings"
	"text/template"

	"github.com/swenske/Janus/dashboard/backend/internal/hypervisor"
)

// What to run on a Proxmox VE node, as root, before the Controller can
// use it: the pool its machines go in, a directory storage for its
// images, a user whose rights cover those and nothing else, and its API
// token (docs/hypervisors.md: preparing a Proxmox host). Every step
// stands alone and can run again.
//
// The rights are the least Proxmox VE 9 accepted for everything the
// Controller does, found by trying - each one added after Proxmox
// refused without it.

// The roles' privileges.
const (
	privsVM      = "VM.Allocate,VM.Audit,VM.Config.CDROM,VM.Config.CPU,VM.Config.Disk,VM.Config.HWType,VM.Config.Memory,VM.Config.Network,VM.Config.Options,VM.Console,VM.PowerMgmt"
	privsDisks   = "Datastore.AllocateSpace,Datastore.Audit"
	privsImages  = "Datastore.Allocate,Datastore.AllocateSpace,Datastore.AllocateTemplate,Datastore.Audit"
	privsNetwork = "SDN.Use"
	privsNode    = "Sys.Audit"
)

type prepData struct {
	Name, Node, User, Token, Pool, Storage, Images, ImagesDir string
	Networks                                                  []prepNetwork
	PrivsVM, PrivsDisks, PrivsImages, PrivsNetwork            string
	PrivsNode                                                 string
}

type prepNetwork struct {
	Name, Path string
	Untagged   bool
}

// HostPreparation gives the steps for c.
func HostPreparation(name string, c *hypervisor.ProxmoxConfig) []hypervisor.PrepStep {
	user, token, _ := strings.Cut(c.TokenID, "!")
	d := prepData{
		Name: hypervisor.CommentSafe(name), Node: c.Node, User: user, Token: token, Pool: c.Pool,
		Storage: c.Storage, Images: c.ImageStorage, ImagesDir: "/var/lib/" + c.ImageStorage,
		PrivsVM: privsVM, PrivsDisks: privsDisks, PrivsImages: privsImages, PrivsNetwork: privsNetwork, PrivsNode: privsNode,
	}
	for _, n := range c.Networks {
		bridge, vlan, err := hypervisor.ParseProxmoxNetwork(n)
		if err != nil {
			continue // Validate refused it already
		}
		pn := prepNetwork{Name: n, Path: "/sdn/zones/localnetwork/" + bridge}
		if vlan > 0 {
			pn.Path += "/" + strings.TrimPrefix(n, bridge+".")
		} else {
			pn.Untagged = true
		}
		d.Networks = append(d.Networks, pn)
	}
	var steps []hypervisor.PrepStep
	for _, s := range prepSteps {
		steps = append(steps, hypervisor.PrepStep{Title: render(s.title, d), About: render(s.about, d), Script: render(s.script, d)})
	}
	return steps
}

func render(text string, d prepData) string {
	var b strings.Builder
	if err := template.Must(template.New("").Parse(text)).Execute(&b, d); err != nil {
		panic(err) // the templates are constants: a test runs them all
	}
	return b.String()
}

var prepSteps = []struct{ title, about, script string }{
	{
		title: "The pool, and the image storage",
		about: "The Controller's machines go in pool {{.Pool}}: the token's rights on virtual machines come from it, so it sees no other one. " +
			"Storage {{.Images}}, a directory, holds the Janus images and the machines' NoCloud volumes - the Controller's own: it may delete its files there.",
		script: `pvesh get /pools/{{.Pool}} >/dev/null 2>&1 || pvesh create /pools --poolid {{.Pool}} --comment "Janus Controller: its machines"
if ! pvesh get /storage/{{.Images}} >/dev/null 2>&1; then
    mkdir -p {{.ImagesDir}}
    pvesm add dir {{.Images}} --path {{.ImagesDir}} --content import,iso --nodes {{.Node}}
fi
pvesh get /storage/{{.Storage}} >/dev/null 2>&1 || echo "warning: there's no storage {{.Storage}} for the machines' disks" >&2
`,
	},
	{
		title: "Roles: what the Controller may do",
		about: "One role per kind of object, each with only what the Controller uses: its machines (in the pool), the disks' storage, the image storage, " +
			"the networks, and reading the node's state.",
		script: `role() { pveum role add "$1" --privs "$2" 2>/dev/null || pveum role modify "$1" --privs "$2"; }
role JanusVM "{{.PrivsVM}}"
role JanusDisks "{{.PrivsDisks}}"
role JanusImages "{{.PrivsImages}}"
role JanusNetwork "{{.PrivsNetwork}}"
role JanusNode "{{.PrivsNode}}"
`,
	},
	{
		title: "The Controller's user and its rights",
		about: "A user with no password - it can't log in to the web interface - and those roles on the pool, the two storages, the networks " +
			"({{range $i, $n := .Networks}}{{if $i}}, {{end}}{{$n.Name}}{{end}}) and node {{.Node}}: nowhere else.",
		script: `pvesh get /access/users/{{.User}} >/dev/null 2>&1 || pveum user add {{.User}} --comment "Janus Controller"
pveum acl modify /pool/{{.Pool}} --users {{.User}} --roles JanusVM
pveum acl modify /storage/{{.Storage}} --users {{.User}} --roles JanusDisks
pveum acl modify /storage/{{.Images}} --users {{.User}} --roles JanusImages
{{- range .Networks}}
pveum acl modify {{.Path}} --users {{$.User}} --roles JanusNetwork{{if .Untagged}} --propagate 0{{end}}
{{- end}}
pveum acl modify /nodes/{{.Node}} --users {{.User}} --roles JanusNode
`,
	},
	{
		title: "The API token",
		about: "The Controller's token, with its user's rights (no separate ones). Its secret is shown once: paste it in the hypervisor's form, " +
			"as the token secret. A token that already exists keeps its secret hidden - remove it and run this again for a new one.",
		script: `if pvesh get /access/users/{{.User}}/token/{{.Token}} >/dev/null 2>&1; then
    echo 'The token {{.User}}!{{.Token}} exists: its secret was shown when it was made. For a new one: pveum user token remove {{.User}} {{.Token}}, then this step again.'
else
    pveum user token add {{.User}} {{.Token}} --privsep 0 --comment "Janus Controller"
fi
`,
	},
	{
		title: "Check",
		about: "What the user may do - on its pool, storages, networks and node only - and the fingerprint of the certificate the API presents: " +
			"the one the Controller must read when it's trusted.",
		script: `pveum user permissions {{.User}}
f=/etc/pve/local/pveproxy-ssl.pem
[ -e "$f" ] || f=/etc/pve/local/pve-ssl.pem
openssl x509 -in "$f" -noout -fingerprint -sha256
`,
	},
}
