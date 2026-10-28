package proxmox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/swenske/Janus/dashboard/backend/internal/hypervisor"
)

// Driver talks to one Proxmox VE node.
type Driver struct {
	cfg          *hypervisor.ProxmoxConfig
	controllerID string
	api          *client
	node         string // path-escaped
}

// New returns a Driver for h, a Proxmox hypervisor whose API
// certificate is trusted.
func New(h *hypervisor.Hypervisor, controllerID string) (*Driver, error) {
	if h.Proxmox == nil {
		return nil, errors.New("not a Proxmox hypervisor")
	}
	if h.TokenSecret == "" {
		return nil, errors.New("no API token secret")
	}
	api, err := newClient(h.Proxmox, h.TokenSecret)
	if err != nil {
		return nil, err
	}
	return &Driver{cfg: h.Proxmox, controllerID: controllerID, api: api, node: url.PathEscape(h.Proxmox.Node)}, nil
}

func (d *Driver) Close() error {
	d.api.http.CloseIdleConnections()
	return nil
}

func (d *Driver) nodePath(format string, args ...any) string {
	return "/nodes/" + d.node + fmt.Sprintf(format, args...)
}

// --- the host ---

func (d *Driver) HostInfo(ctx context.Context) (*hypervisor.HostInfo, error) {
	var st struct {
		CPU     float64 `json:"cpu"`
		CPUInfo struct {
			Model string `json:"model"`
			CPUs  int    `json:"cpus"`
			MHz   string `json:"mhz"`
		} `json:"cpuinfo"`
		Memory struct {
			Total     uint64 `json:"total"`
			Available uint64 `json:"available"`
			Free      uint64 `json:"free"`
		} `json:"memory"`
		PVEVersion    string `json:"pveversion"`
		CurrentKernel struct {
			Release string `json:"release"`
		} `json:"current-kernel"`
	}
	if err := d.api.do(ctx, http.MethodGet, d.nodePath("/status"), nil, &st); err != nil {
		return nil, err
	}
	mhz, _ := strconv.ParseFloat(st.CPUInfo.MHz, 64)
	free := st.Memory.Available
	if free == 0 {
		free = st.Memory.Free
	}
	usage := st.CPU
	info := &hypervisor.HostInfo{
		Hostname:          d.cfg.Node,
		HostID:            hostID(d.cfg.Node),
		HypervisorVersion: "Proxmox VE " + pveVersion(st.PVEVersion),
		LibraryVersion:    "Linux " + st.CurrentKernel.Release,
		CPUModel:          st.CPUInfo.Model,
		CPUs:              st.CPUInfo.CPUs,
		CPUMHz:            int(mhz),
		CPUUsage:          &usage,
		MemoryTotal:       st.Memory.Total,
		MemoryFree:        free,
		Storage:           d.storageInfo(ctx, d.cfg.Storage),
		RunningMachines:   -1,
	}
	images := d.imageStorageInfo(ctx)
	info.ImageStorage = &images
	bridges := map[string]bool{}
	var ifaces []struct {
		Iface  string `json:"iface"`
		Active int    `json:"active"`
	}
	netErr := d.api.do(ctx, http.MethodGet, d.nodePath("/network"), url.Values{"type": {"any_bridge"}}, &ifaces)
	for _, i := range ifaces {
		bridges[i.Iface] = i.Active == 1
	}
	for _, n := range d.cfg.Networks {
		ni := hypervisor.NetworkInfo{Name: n}
		bridge, _, _, err := hypervisor.ParseProxmoxAllowed(n)
		switch {
		case err != nil:
			ni.Error = err.Error()
		case netErr != nil:
			ni.Error = netErr.Error()
		default:
			active, ok := bridges[bridge]
			if !ok {
				ni.Error = "no bridge " + bridge
			}
			ni.Active = active
		}
		info.Networks = append(info.Networks, ni)
	}
	// The token sees its pool's machines only.
	var vms []struct {
		Status string `json:"status"`
	}
	if err := d.api.do(ctx, http.MethodGet, d.nodePath("/qemu"), nil, &vms); err == nil {
		info.RunningMachines = 0
		for _, v := range vms {
			if v.Status == "running" {
				info.RunningMachines++
			}
		}
	}
	return info, nil
}

// pveVersion is "9.2.21" from "pve-manager/9.2.21/4f6e0ac8".
func pveVersion(s string) string {
	parts := strings.Split(s, "/")
	if len(parts) > 1 {
		return parts[1]
	}
	return s
}

func (d *Driver) storageInfo(ctx context.Context, storage string) hypervisor.StorageInfo {
	si := hypervisor.StorageInfo{Name: storage}
	var st struct {
		Active int    `json:"active"`
		Total  uint64 `json:"total"`
		Used   uint64 `json:"used"`
		Avail  uint64 `json:"avail"`
	}
	if err := d.api.do(ctx, http.MethodGet, d.nodePath("/storage/%s/status", url.PathEscape(storage)), nil, &st); err != nil {
		si.Error = err.Error()
		return si
	}
	si.Active, si.Capacity, si.Allocated, si.Available = st.Active == 1, st.Total, st.Used, st.Avail
	return si
}

// blockStorages are Proxmox storage types without files: an upload is
// refused there ("can't upload to storage type 'lvmthin'").
var blockStorages = map[string]bool{"lvm": true, "lvmthin": true, "zfspool": true, "rbd": true, "iscsi": true, "iscsidirect": true}

// imageStorageProblem is why storage name, of type typ holding content
// (Proxmox's comma-separated list), can't keep the base images and the
// NoCloud volumes - "" when it can: a file storage (an upload goes to a
// file) with the import and iso content types.
func imageStorageProblem(name, typ, content string) string {
	if blockStorages[typ] {
		return fmt.Sprintf("its type, %s, takes no upload: the image storage must be a directory (or NFS, CIFS...) storage with the import and iso content types - the host preparation makes one: pvesm add dir janus-images --path /var/lib/janus-images --content import,iso", typ)
	}
	have := map[string]bool{}
	for _, c := range strings.Split(content, ",") {
		have[strings.TrimSpace(c)] = true
	}
	var missing []string
	for _, c := range []string{"import", "iso"} {
		if !have[c] {
			missing = append(missing, c)
		}
	}
	if len(missing) > 0 {
		return fmt.Sprintf("it doesn't hold %s content: pvesm set %s --content %s", strings.Join(missing, " nor "), name, strings.Trim(content+","+strings.Join(missing, ","), ","))
	}
	return ""
}

// imageStorageInfo is the image storage as the base images see it: its
// space, and why it can't take them.
func (d *Driver) imageStorageInfo(ctx context.Context) hypervisor.StorageInfo {
	si := hypervisor.StorageInfo{Name: d.cfg.ImageStorage}
	var st struct {
		Active  int    `json:"active"`
		Total   uint64 `json:"total"`
		Used    uint64 `json:"used"`
		Avail   uint64 `json:"avail"`
		Type    string `json:"type"`
		Content string `json:"content"`
	}
	if err := d.api.do(ctx, http.MethodGet, d.nodePath("/storage/%s/status", url.PathEscape(d.cfg.ImageStorage)), nil, &st); err != nil {
		si.Error = err.Error()
		return si
	}
	si.Active, si.Capacity, si.Allocated, si.Available = st.Active == 1, st.Total, st.Used, st.Avail
	si.Error = imageStorageProblem(d.cfg.ImageStorage, st.Type, st.Content)
	return si
}

// --- images and NoCloud volumes, on the image storage ---

func (d *Driver) volume(content, name string) string {
	return d.cfg.ImageStorage + ":" + content + "/" + name
}

func (d *Driver) HasImage(ctx context.Context, img hypervisor.Image) (bool, error) {
	// Said before anything is downloaded, rather than by the upload.
	if si := d.imageStorageInfo(ctx); si.Error != "" {
		return false, fmt.Errorf("the image storage %s: %s", si.Name, si.Error)
	}
	var vols []struct {
		VolID string `json:"volid"`
	}
	if err := d.api.do(ctx, http.MethodGet, d.nodePath("/storage/%s/content", url.PathEscape(d.cfg.ImageStorage)), url.Values{"content": {"import"}}, &vols); err != nil {
		return false, err
	}
	want := d.volume("import", img.Name)
	for _, v := range vols {
		if v.VolID == want {
			return true, nil
		}
	}
	return false, nil
}

// UploadImage stores the image as import content: Proxmox writes it
// under a temporary name and only moves it into place once complete.
func (d *Driver) UploadImage(ctx context.Context, img hypervisor.Image, r io.Reader) error {
	return d.upload(ctx, "import", img.Name, img.Size, r)
}

// upload streams size bytes from r as a file of the image storage.
func (d *Driver) upload(ctx context.Context, content, name string, size int64, r io.Reader) error {
	// The multipart body's length is known up front - Proxmox reads the
	// fields first, the file last, with the checksum's algorithm before
	// the checksum itself.
	var head bytes.Buffer
	mw := multipart.NewWriter(&head)
	if err := mw.WriteField("content", content); err != nil {
		return err
	}
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="filename"; filename=%q`, name))
	h.Set("Content-Type", "application/octet-stream")
	if _, err := mw.CreatePart(h); err != nil {
		return err
	}
	tail := "\r\n--" + mw.Boundary() + "--\r\n"
	body := io.MultiReader(bytes.NewReader(head.Bytes()), io.LimitReader(r, size), strings.NewReader(tail))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.api.base+d.nodePath("/storage/%s/upload", url.PathEscape(d.cfg.ImageStorage)), body)
	if err != nil {
		return err
	}
	req.ContentLength = int64(head.Len()) + size + int64(len(tail))
	req.Header.Set("Content-Type", mw.FormDataContentType())
	var upid string
	if err := d.api.send(req, &upid); err != nil {
		return fmt.Errorf("upload %s: %w", name, err)
	}
	if err := d.api.wait(ctx, upid); err != nil {
		return fmt.Errorf("upload %s: %w", name, err)
	}
	return nil
}

func (d *Driver) deleteVolume(ctx context.Context, volid string) error {
	err := d.api.task(ctx, http.MethodDelete, d.nodePath("/storage/%s/content/%s", url.PathEscape(d.cfg.ImageStorage), url.PathEscape(volid)), nil)
	if err != nil && !isMissing(err) {
		return err
	}
	return nil
}

// --- machines ---

func ciDataName(spec hypervisor.MachineSpec) string {
	return "janus-cidata-" + spec.MachineID + ".iso"
}

// ownsVolumeName: a volume CreateMachine made for the machine carries its
// ID in its name.
func ownsVolumeName(volid, machineID string) bool {
	return machineID != "" && strings.Contains(volid, "janus-cidata-"+machineID+".iso")
}

func (d *Driver) Plan(spec hypervisor.MachineSpec) hypervisor.MachineRef {
	return hypervisor.MachineRef{MachineID: spec.MachineID, Name: spec.Name, Volumes: []string{d.volume("iso", ciDataName(spec))}}
}

// tag is what a machine's description says about who created it.
func (d *Driver) tag(machineID string) string {
	return "janus-controller=" + d.controllerID + " janus-machine=" + machineID
}

var tagRe = regexp.MustCompile(`janus-controller=(\S+) janus-machine=(\S+)`)

// description is a machine's notes in Proxmox: what it is, and its
// ownership tag.
func (d *Driver) description(spec hypervisor.MachineSpec) string {
	return fmt.Sprintf("Janus node %s, created by a Janus Controller - change it there, not here.\n\n%s", spec.Name, d.tag(spec.MachineID))
}

// nicParam is a network interface as Proxmox writes it.
func nicParam(n hypervisor.NIC) (string, error) {
	bridge, vlan, err := hypervisor.ParseProxmoxNetwork(n.Network)
	if err != nil {
		return "", err
	}
	p := "virtio=" + strings.ToUpper(n.MAC) + ",bridge=" + bridge
	if vlan > 0 {
		p += ",tag=" + strconv.Itoa(vlan)
	}
	return p, nil
}

func (d *Driver) CreateMachine(ctx context.Context, spec hypervisor.MachineSpec) (*hypervisor.MachineRef, error) {
	ref := d.Plan(spec)
	for _, n := range spec.NICs {
		if !d.cfg.AllowsNetwork(n.Network) {
			return nil, fmt.Errorf("network %s isn't one this hypervisor allows", n.Network)
		}
	}
	if _, err := d.find(ctx, ref); err == nil {
		return nil, fmt.Errorf("a virtual machine named %s already exists on this hypervisor", spec.Name)
	} else if !errors.Is(err, hypervisor.ErrNotFound) {
		return nil, err
	}

	ci := ciDataName(spec)
	if err := d.upload(ctx, "iso", ci, int64(len(spec.CIData)), bytes.NewReader(spec.CIData)); err != nil {
		return nil, err
	}
	if err := checkHardware(spec.Hardware); err != nil {
		return nil, err
	}
	params := d.createParams(spec, ci)
	for i, n := range spec.NICs {
		p, err := nicParam(n)
		if err != nil {
			_ = d.deleteVolume(ctx, d.volume("iso", ci))
			return nil, err
		}
		params.Set(fmt.Sprintf("net%d", i), p)
	}
	vmid, err := d.createVM(ctx, params)
	if err != nil {
		_ = d.deleteVolume(ctx, d.volume("iso", ci))
		return nil, err
	}
	ref.UUID = strconv.Itoa(vmid)
	vm := d.nodePath("/qemu/%d", vmid)
	// Tags only once the machine is in its pool: Proxmox checks them
	// against the machine's own rights, not the pool's (GuestHelpers'
	// assert_tag_permissions) - at creation it has none yet.
	_ = d.api.do(ctx, http.MethodPut, vm+"/config", url.Values{"tags": {"janus"}}, nil)
	if err := d.api.task(ctx, http.MethodPost, vm+"/status/start", nil); err != nil {
		_ = d.destroy(ctx, vmid)
		_ = d.deleteVolume(ctx, d.volume("iso", ci))
		return nil, fmt.Errorf("start the virtual machine: %w", err)
	}
	return &ref, nil
}

// cpuModel is the CPU model a machine gets: the one Proxmox VE's own
// form fills in for a new machine, while its API leaves kvm64 when it's
// not given - no AES-NI, no SSE4 or AVX: HAProxy's AWS-LC fell back to
// ChaCha20 and generic code for every curve and hash (#372). x86-64-v2-AES
// runs on any x86-64-v2 host with AES, so a machine still migrates
// between a cluster's hosts.
const cpuModel = "x86-64-v2-AES"

// agentParam turns the guest agent on, on its virtio-serial port - the
// one the qemu-guest-agent extension listens on
// (/dev/virtio-ports/org.qemu.guest_agent.0).
const agentParam = "enabled=1,type=virtio"

// createParams is the new machine's configuration but its network
// interfaces.
func (d *Driver) createParams(spec hypervisor.MachineSpec, ci string) url.Values {
	params := url.Values{
		"name":        {spec.Name},
		"pool":        {d.cfg.Pool},
		"machine":     {"q35"},
		"bios":        {"ovmf"},
		"cpu":         {cpuModel},
		"efidisk0":    {d.cfg.Storage + ":1,efitype=4m,pre-enrolled-keys=0"},
		"scsihw":      {"virtio-scsi-single"},
		"virtio0":     {d.cfg.Storage + ":0,import-from=" + d.volume("import", spec.Image.Name)},
		"ide2":        {d.volume("iso", ci) + ",media=cdrom"},
		"serial0":     {"socket"},
		"vga":         {"std"},
		"boot":        {"order=virtio0"},
		"onboot":      {"1"},
		"ostype":      {"l26"},
		"description": {d.description(spec)},
	}
	// Only with an agent in the image: on without one, Proxmox's
	// shutdowns and backups wait for it and time out (#373).
	if spec.GuestAgent {
		params.Set("agent", agentParam)
	}
	hw, _ := hardwareParams(nil, spec.Hardware)
	for k, v := range hw {
		params[k] = v
	}
	return params
}

// option reads a property string option ("x86-64-v2-AES",
// "cputype=host,flags=+aes", "1,fstrim_cloned_disks=1"): its value for
// key - a first part without a name is the value of the option's
// default key, def.
func option(raw any, def, key string) string {
	if raw == nil {
		return ""
	}
	for i, part := range strings.Split(fmt.Sprint(raw), ",") {
		k, v, named := strings.Cut(strings.TrimSpace(part), "=")
		if !named && i == 0 {
			k, v = def, k
		}
		if k == key {
			return v
		}
	}
	return ""
}

// cpuDrift is a machine's CPU model when it's one Proxmox VE gives a
// machine created without any - kvm64 (qemu64 beside it): no AES-NI,
// SSE4, AVX, BMI2 or ADX (#372) - an older Controller's machines. A model
// someone chose (host, x86-64-v3...) is theirs, left alone.
func cpuDrift(cfg map[string]any) *hypervisor.Drift {
	have := option(cfg["cpu"], "cputype", "cputype")
	switch have {
	case "", "kvm64", "qemu64":
		return &hypervisor.Drift{Setting: "cpu", Have: or(have, "kvm64 (Proxmox's default)"), Want: cpuModel}
	}
	return nil
}

// agentOn reports whether a machine's agent option has the guest agent
// on, on its virtio-serial port.
func agentOn(cfg map[string]any) bool {
	v, ok := cfg["agent"]
	if !ok || v == nil {
		return false
	}
	typ := option(v, "enabled", "type")
	return option(v, "enabled", "enabled") == "1" && (typ == "" || typ == "virtio")
}

// settingsParams is what Reconfigure changes so the machine has s and
// today's CPU model - only what differs -, and the keys to delete.
func settingsParams(cfg map[string]any, s hypervisor.Settings) (url.Values, []string) {
	p := url.Values{}
	var deletes []string
	if cpuDrift(cfg) != nil {
		p.Set("cpu", cpuModel)
	}
	switch on := agentOn(cfg); {
	case s.GuestAgent && !on:
		p.Set("agent", agentParam)
	case !s.GuestAgent && cfg["agent"] != nil:
		deletes = append(deletes, "agent")
	}
	return p, deletes
}

func or(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// createVM creates the machine under the first free ID - of the
// configured range, or the cluster's next one - trying the next one if
// another creation took it meanwhile.
func (d *Driver) createVM(ctx context.Context, params url.Values) (int, error) {
	first, last, ranged := d.cfg.VMIDRange()
	next := first
	for attempt := 0; attempt < 20; attempt++ {
		var vmid int
		if ranged {
			id, err := d.freeVMID(ctx, next, last)
			if err != nil {
				return 0, err
			}
			vmid = id
		} else {
			var s json.Number
			if err := d.api.do(ctx, http.MethodGet, "/cluster/nextid", nil, &s); err != nil {
				return 0, err
			}
			id, err := strconv.Atoi(s.String())
			if err != nil {
				return 0, fmt.Errorf("next VM ID %q isn't a number", s)
			}
			vmid = id
		}
		p := url.Values{}
		for k, v := range params {
			p[k] = v
		}
		p.Set("vmid", strconv.Itoa(vmid))
		err := d.api.task(ctx, http.MethodPost, d.nodePath("/qemu"), p)
		if err == nil {
			return vmid, nil
		}
		if !strings.Contains(err.Error(), "already exists") {
			return 0, fmt.Errorf("create the virtual machine %d: %w", vmid, err)
		}
		next = vmid + 1
	}
	return 0, errors.New("no free VM ID found")
}

// freeVMID is the first VM ID from first to last nobody uses - the
// cluster says, whatever the token may see.
func (d *Driver) freeVMID(ctx context.Context, first, last int) (int, error) {
	for id := first; id <= last; id++ {
		var s json.Number
		err := d.api.do(ctx, http.MethodGet, "/cluster/nextid", url.Values{"vmid": {strconv.Itoa(id)}}, &s)
		if err == nil {
			return id, nil
		}
		var e *apiError
		if !errors.As(err, &e) || e.Status != http.StatusBadRequest {
			return 0, err
		}
	}
	return 0, fmt.Errorf("every VM ID from %d to %d is taken", first, last)
}

type vmConfig struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	CPU         string `json:"cpu"`
	Cores       int    `json:"cores"`
	Sockets     int    `json:"sockets"`
	Vcpus       int    `json:"vcpus"`
	Numa        int    `json:"numa"`
	Hotplug     string `json:"hotplug"`
	Memory      any    `json:"memory"` // a number, or a string since Proxmox VE 9
	raw         map[string]any
}

// configOn is VM vmid's configuration, read on node - where it runs.
func (d *Driver) configOn(ctx context.Context, node string, vmid int) (*vmConfig, error) {
	var raw map[string]any
	// current=1: what the machine has, not changes pending for its next
	// start.
	if err := d.api.do(ctx, http.MethodGet, "/nodes/"+url.PathEscape(node)+fmt.Sprintf("/qemu/%d/config", vmid), url.Values{"current": {"1"}}, &raw); err != nil {
		if isPermission(err) || isMissing(err) {
			return nil, hypervisor.ErrNotFound
		}
		return nil, err
	}
	b, _ := json.Marshal(raw)
	var c vmConfig
	_ = json.Unmarshal(b, &c)
	c.raw = raw
	return &c, nil
}

// memoryMiB reads the memory setting: "2048", 2048 or "current=2048".
func (c *vmConfig) memoryMiB() int {
	switch v := c.Memory.(type) {
	case float64:
		return int(v)
	case string:
		for _, part := range strings.Split(v, ",") {
			part = strings.TrimPrefix(part, "current=")
			if n, err := strconv.Atoi(part); err == nil {
				return n
			}
		}
	}
	return 0
}

// owned is ref's VM ID, once its description shows it's this
// Controller's machine ref.MachineID. A machine the token can't see -
// gone, or outside its pool - is ErrNotFound: there's nothing to do to
// it.
func (d *Driver) owned(ctx context.Context, ref hypervisor.MachineRef) (int, *vmConfig, error) {
	vmid, err := d.find(ctx, ref)
	if err != nil {
		return 0, nil, err
	}
	return d.ownedOn(ctx, d.cfg.Node, vmid, ref)
}

// ownedOn checks VM vmid, on node, is ref's.
func (d *Driver) ownedOn(ctx context.Context, node string, vmid int, ref hypervisor.MachineRef) (int, *vmConfig, error) {
	c, err := d.configOn(ctx, node, vmid)
	if err != nil {
		return 0, nil, err
	}
	m := tagRe.FindStringSubmatch(c.Description)
	if m == nil || m[1] != d.controllerID || m[2] != ref.MachineID {
		return 0, nil, hypervisor.ErrNotOwned
	}
	return vmid, c, nil
}

// find is ref's VM ID: its recorded one, or - for a machine whose
// creation was interrupted - the one with its name among those the
// token may see.
func (d *Driver) find(ctx context.Context, ref hypervisor.MachineRef) (int, error) {
	if ref.UUID != "" {
		return strconv.Atoi(ref.UUID)
	}
	var vms []struct {
		VMID int    `json:"vmid"`
		Name string `json:"name"`
	}
	if err := d.api.do(ctx, http.MethodGet, d.nodePath("/qemu"), nil, &vms); err != nil {
		return 0, err
	}
	for _, v := range vms {
		if v.Name == ref.Name {
			return v.VMID, nil
		}
	}
	return 0, hypervisor.ErrNotFound
}

// hostID is a Proxmox node's HostInfo.HostID: its name - two separate
// Proxmox installations whose nodes share a name count as one host, a
// warning more, never two members placed together unseen.
func hostID(node string) string { return "proxmox:" + node }

// nodeOf is the node VM vmid runs on now - an HA migration moves it -,
// from the cluster's resources the token may see; the record's node
// when it can't be told.
func (d *Driver) nodeOf(ctx context.Context, vmid int) string {
	var vms []struct {
		VMID int    `json:"vmid"`
		Node string `json:"node"`
	}
	if err := d.api.do(ctx, http.MethodGet, "/cluster/resources", url.Values{"type": {"vm"}}, &vms); err == nil {
		for _, v := range vms {
			if v.VMID == vmid && v.Node != "" {
				return v.Node
			}
		}
	}
	return d.cfg.Node
}

func (d *Driver) MachineStatus(ctx context.Context, ref hypervisor.MachineRef) (*hypervisor.MachineStatus, error) {
	vmid, err := d.find(ctx, ref)
	if err != nil {
		return nil, err
	}
	node := d.nodeOf(ctx, vmid)
	vmid, c, err := d.ownedOn(ctx, node, vmid, ref)
	if err != nil {
		return nil, err
	}
	var st struct {
		Status    string `json:"status"`
		QMPStatus string `json:"qmpstatus"`
		CPUs      int    `json:"cpus"`
		MaxMem    uint64 `json:"maxmem"`
	}
	if err := d.api.do(ctx, http.MethodGet, "/nodes/"+url.PathEscape(node)+fmt.Sprintf("/qemu/%d/status/current", vmid), nil, &st); err != nil {
		if isPermission(err) || isMissing(err) {
			return nil, hypervisor.ErrNotFound
		}
		return nil, err
	}
	agent := agentOn(c.raw)
	out := &hypervisor.MachineStatus{Power: powerState(st.Status, st.QMPStatus), HostID: hostID(node), HostName: node, GuestAgent: &agent}
	out.VCPUs, out.MaxVCPUs, out.MemoryMiB, out.MaxMemoryMiB, out.MemoryStepMiB = statusOf(c)
	if d := cpuDrift(c.raw); d != nil {
		out.Drift = append(out.Drift, *d)
	}
	return out, nil
}

func powerState(status, qmp string) hypervisor.PowerState {
	switch {
	case status == "running" && (qmp == "paused" || qmp == "suspended"):
		return hypervisor.PowerPaused
	case status == "running":
		return hypervisor.PowerRunning
	case status == "stopped":
		return hypervisor.PowerOff
	}
	return hypervisor.PowerUnknown
}

func (d *Driver) Power(ctx context.Context, ref hypervisor.MachineRef, action hypervisor.PowerAction) error {
	vmid, _, err := d.owned(ctx, ref)
	if err != nil {
		return err
	}
	op := map[hypervisor.PowerAction]string{hypervisor.PowerStart: "start", hypervisor.PowerForceOff: "stop", hypervisor.PowerReset: "reset"}[action]
	if op == "" {
		return fmt.Errorf("unknown power action %q", action)
	}
	return d.api.task(ctx, http.MethodPost, d.nodePath("/qemu/%d/status/%s", vmid, op), nil)
}

// CheckHardware says why a Proxmox VE machine can't be made of hw - its
// memory modules above all -, or nil.
func CheckHardware(hw hypervisor.Hardware) error { return checkHardware(hw) }

// Reconfigure changes a stopped machine's size, maxima, interfaces and
// settings: what it read with current=1 is what it runs with, nothing
// pending - stopped, the change applies at once, and its next start
// runs with it.
func (d *Driver) Reconfigure(ctx context.Context, ref hypervisor.MachineRef, hw hypervisor.Hardware, nics []hypervisor.NIC, s hypervisor.Settings) error {
	if err := checkHardware(hw); err != nil {
		return err
	}
	vmid, c, err := d.owned(ctx, ref)
	if err != nil {
		return err
	}
	var st struct {
		Status string `json:"status"`
	}
	if err := d.api.do(ctx, http.MethodGet, d.nodePath("/qemu/%d/status/current", vmid), nil, &st); err != nil {
		return err
	}
	if st.Status != "stopped" {
		return fmt.Errorf("the virtual machine is %s - it must be stopped first", st.Status)
	}
	for _, n := range nics {
		if !d.cfg.AllowsNetwork(n.Network) {
			return fmt.Errorf("network %s isn't one this hypervisor allows", n.Network)
		}
	}
	params, err := reconfigureParams(c.raw, hw, nics, s)
	if err != nil {
		return err
	}
	return d.api.do(ctx, http.MethodPut, d.nodePath("/qemu/%d/config", vmid), params, nil)
}

// Resize plugs or unplugs a running scalable machine's vCPUs and memory
// modules: a configuration change Proxmox applies at once with hot-plug
// on. One it couldn't is left pending for the next start - it's taken
// back, and an error.
func (d *Driver) Resize(ctx context.Context, ref hypervisor.MachineRef, vcpus, memoryMiB int) error {
	vmid, c, err := d.owned(ctx, ref)
	if err != nil {
		return err
	}
	var st struct {
		Status string `json:"status"`
	}
	if err := d.api.do(ctx, http.MethodGet, d.nodePath("/qemu/%d/status/current", vmid), nil, &st); err != nil {
		return err
	}
	if st.Status != "running" {
		return fmt.Errorf("the virtual machine is %s: it's resized through its hardware instead", st.Status)
	}
	curV, maxV, curM, _, step := statusOf(c)
	p := url.Values{}
	if vcpus != curV {
		if !c.hotplugs("cpu") || vcpus < 1 || vcpus > maxV {
			return fmt.Errorf("%d vCPUs: this machine takes 1 to %d without a restart", vcpus, maxV)
		}
		p.Set("vcpus", strconv.Itoa(vcpus))
	}
	if memoryMiB != curM {
		if step == 0 || !hotplugMemoryAligned(memoryMiB) {
			return fmt.Errorf("%d MiB: this machine's memory changes by %d MiB modules above %d MiB while it runs", memoryMiB, dimmMiB, staticMemoryMiB)
		}
		p.Set("memory", strconv.Itoa(memoryMiB))
	}
	if len(p) == 0 {
		return nil
	}
	path := d.nodePath("/qemu/%d/config", vmid)
	// Memory first: a vCPU plugged in makes the node reload HAProxy,
	// which needs room for a second process for a while.
	for _, key := range []string{"memory", "vcpus"} {
		if p.Get(key) == "" {
			continue
		}
		if err := d.api.do(ctx, http.MethodPut, path, url.Values{key: {p.Get(key)}}, nil); err != nil {
			return fmt.Errorf("resize %s: %w", key, err)
		}
	}
	var pending []struct {
		Key     string `json:"key"`
		Pending any    `json:"pending"`
	}
	if err := d.api.do(ctx, http.MethodGet, d.nodePath("/qemu/%d/pending", vmid), nil, &pending); err != nil {
		return fmt.Errorf("read what's pending: %w", err)
	}
	var stuck []string
	for _, e := range pending {
		if e.Pending != nil && (e.Key == "vcpus" || e.Key == "memory") {
			stuck = append(stuck, e.Key)
		}
	}
	if len(stuck) > 0 {
		_ = d.api.do(ctx, http.MethodPut, path, url.Values{"revert": {strings.Join(stuck, ",")}}, nil)
		return fmt.Errorf("the hypervisor couldn't change %s while the machine runs (left for its next start, taken back)", strings.Join(stuck, " and "))
	}
	return nil
}

var netKeyRe = regexp.MustCompile(`^net([0-9]+)$`)
var macRe = regexp.MustCompile(`(?i)(?:virtio|e1000e?|vmxnet3|rtl8139)=([0-9a-f:]{17})`)

// reconfigureParams is the config change for vCPUs, memory, the
// settings (settingsParams) and the whole new set of interfaces, matched
// by MAC: one gone is deleted, one whose network changed is rewritten
// (same slot, same MAC), one new takes the first free slot.
func reconfigureParams(cfg map[string]any, hw hypervisor.Hardware, nics []hypervisor.NIC, s hypervisor.Settings) (url.Values, error) {
	p, hwDeletes := hardwareParams(cfg, hw)
	sp, settingsDeletes := settingsParams(cfg, s)
	for k, v := range sp {
		p[k] = v
	}
	hwDeletes = append(hwDeletes, settingsDeletes...)
	slots := map[string]int{} // MAC -> slot
	used := map[int]bool{}
	for k, v := range cfg {
		m := netKeyRe.FindStringSubmatch(k)
		if m == nil {
			continue
		}
		slot, _ := strconv.Atoi(m[1])
		used[slot] = true
		if mac := macRe.FindStringSubmatch(fmt.Sprint(v)); mac != nil {
			slots[strings.ToLower(mac[1])] = slot
		}
	}
	want := map[string]bool{}
	deletes := hwDeletes
	for _, n := range nics {
		want[strings.ToLower(n.MAC)] = true
	}
	for mac, slot := range slots {
		if !want[mac] {
			deletes = append(deletes, fmt.Sprintf("net%d", slot))
			delete(used, slot)
		}
	}
	for _, n := range nics {
		param, err := nicParam(n)
		if err != nil {
			return nil, err
		}
		slot, ok := slots[strings.ToLower(n.MAC)]
		if !ok {
			for slot = 0; used[slot]; slot++ {
			}
			used[slot] = true
		}
		p.Set(fmt.Sprintf("net%d", slot), param)
	}
	if len(deletes) > 0 {
		sort.Strings(deletes)
		p.Set("delete", strings.Join(deletes, ","))
	}
	return p, nil
}

func (d *Driver) DestroyMachine(ctx context.Context, ref hypervisor.MachineRef) error {
	vmid, _, err := d.owned(ctx, ref)
	switch {
	case errors.Is(err, hypervisor.ErrNotFound):
		// Already gone (or never created): only its volumes are left.
	case err != nil:
		return err
	default:
		if err := d.destroy(ctx, vmid); err != nil {
			return err
		}
	}
	for _, v := range ref.Volumes {
		if !ownsVolumeName(v, ref.MachineID) || !strings.HasPrefix(v, d.cfg.ImageStorage+":") {
			continue
		}
		if err := d.deleteVolume(ctx, v); err != nil {
			return fmt.Errorf("remove %s: %w", v, err)
		}
	}
	return nil
}

// destroy stops and removes a machine with its disks.
func (d *Driver) destroy(ctx context.Context, vmid int) error {
	vm := d.nodePath("/qemu/%d", vmid)
	var st struct {
		Status string `json:"status"`
	}
	if err := d.api.do(ctx, http.MethodGet, vm+"/status/current", nil, &st); err == nil && st.Status != "stopped" {
		if err := d.api.task(ctx, http.MethodPost, vm+"/status/stop", nil); err != nil {
			return fmt.Errorf("stop the virtual machine: %w", err)
		}
	}
	if err := d.api.task(ctx, http.MethodDelete, vm, url.Values{"purge": {"1"}, "destroy-unreferenced-disks": {"1"}}); err != nil && !isMissing(err) {
		return fmt.Errorf("remove the virtual machine: %w", err)
	}
	return nil
}
