package proxmox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/swenske/Janus/dashboard/backend/internal/hypervisor"
)

// Console streams the machine's serial console (serial0) the way
// Proxmox's own xterm.js console does: termproxy runs "qm terminal" on
// the node, and its output comes over a websocket. Read-only: nothing is
// ever sent but the ticket and keepalives. One reader at a time, like
// the serial socket itself - the Controller shares it (consoleHub).
func (d *Driver) Console(ctx context.Context, ref hypervisor.MachineRef, w io.Writer) error {
	vmid, _, err := d.owned(ctx, ref)
	if err != nil {
		return err
	}
	var tp struct {
		Port   json.Number `json:"port"`
		Ticket string      `json:"ticket"`
		User   string      `json:"user"`
	}
	if err := d.api.do(ctx, http.MethodPost, d.nodePath("/qemu/%d/termproxy", vmid), url.Values{"serial": {"serial0"}}, &tp); err != nil {
		return fmt.Errorf("open the console: %w", err)
	}
	wsURL := "wss" + strings.TrimPrefix(d.api.base, "https") + d.nodePath("/qemu/%d/vncwebsocket", vmid) +
		"?" + url.Values{"port": {tp.Port.String()}, "vncticket": {tp.Ticket}}.Encode()
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPClient: d.api.http,
		HTTPHeader: http.Header{"Authorization": {d.api.auth}},
	})
	if err != nil {
		return fmt.Errorf("open the console: %w", err)
	}
	defer func() { _ = conn.CloseNow() }()
	conn.SetReadLimit(1 << 20)
	if err := conn.Write(ctx, websocket.MessageText, []byte(tp.User+":"+tp.Ticket+"\n")); err != nil {
		return fmt.Errorf("open the console: %w", err)
	}
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				_ = conn.Write(ctx, websocket.MessageText, []byte("2")) // keepalive
			}
		}
	}()
	open := false
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("the console closed: %w", err)
		}
		if !open {
			if !bytes.HasPrefix(data, []byte("OK")) {
				return fmt.Errorf("the console was refused: %q", data)
			}
			open, data = true, data[2:]
		}
		if len(data) > 0 {
			if _, err := w.Write(data); err != nil {
				return err
			}
		}
	}
}
