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

// --- images and NoCloud volumes, on the image storage ---

func (d *Driver) volume(content, name string) string {
	return d.cfg.ImageStorage + ":" + content + "/" + name
}

func (d *Driver) HasImage(ctx context.Context, img hypervisor.Image) (bool, error) {
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
	params := url.Values{
		"name":        {spec.Name},
		"pool":        {d.cfg.Pool},
		"cores":       {strconv.Itoa(spec.VCPUs)},
		"memory":      {strconv.Itoa(spec.MemoryMiB)},
		"machine":     {"q35"},
		"bios":        {"ovmf"},
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
	Cores       int    `json:"cores"`
	Sockets     int    `json:"sockets"`
	Memory      any    `json:"memory"` // a number, or a string since Proxmox VE 9
	raw         map[string]any
}

func (d *Driver) config(ctx context.Context, vmid int) (*vmConfig, error) {
	var raw map[string]any
	if err := d.api.do(ctx, http.MethodGet, d.nodePath("/qemu/%d/config", vmid), nil, &raw); err != nil {
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
	c, err := d.config(ctx, vmid)
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

func (d *Driver) MachineStatus(ctx context.Context, ref hypervisor.MachineRef) (*hypervisor.MachineStatus, error) {
	vmid, c, err := d.owned(ctx, ref)
	if err != nil {
		return nil, err
	}
	var st struct {
		Status    string `json:"status"`
		QMPStatus string `json:"qmpstatus"`
		CPUs      int    `json:"cpus"`
		MaxMem    uint64 `json:"maxmem"`
	}
	if err := d.api.do(ctx, http.MethodGet, d.nodePath("/qemu/%d/status/current", vmid), nil, &st); err != nil {
		if isPermission(err) || isMissing(err) {
			return nil, hypervisor.ErrNotFound
		}
		return nil, err
	}
	out := &hypervisor.MachineStatus{Power: powerState(st.Status, st.QMPStatus), VCPUs: c.Cores * max(c.Sockets, 1), MemoryMiB: c.memoryMiB()}
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

// Reconfigure changes a stopped machine's vCPUs, memory and interfaces.
func (d *Driver) Reconfigure(ctx context.Context, ref hypervisor.MachineRef, vcpus, memoryMiB int, nics []hypervisor.NIC) error {
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
	params, err := reconfigureParams(c.raw, vcpus, memoryMiB, nics)
	if err != nil {
		return err
	}
	return d.api.do(ctx, http.MethodPut, d.nodePath("/qemu/%d/config", vmid), params, nil)
}

var netKeyRe = regexp.MustCompile(`^net([0-9]+)$`)
var macRe = regexp.MustCompile(`(?i)(?:virtio|e1000e?|vmxnet3|rtl8139)=([0-9a-f:]{17})`)

// reconfigureParams is the config change for vCPUs, memory and the
// whole new set of interfaces, matched by MAC: one gone is deleted, one
// whose network changed is rewritten (same slot, same MAC), one new
// takes the first free slot.
func reconfigureParams(cfg map[string]any, vcpus, memoryMiB int, nics []hypervisor.NIC) (url.Values, error) {
	p := url.Values{"cores": {strconv.Itoa(vcpus)}, "sockets": {"1"}, "memory": {strconv.Itoa(memoryMiB)}}
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
	var deletes []string
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
