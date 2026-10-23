package acmeorder

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"time"

	"github.com/swenske/Janus/internal/acme/acmewire"
)

// RunClient has janus-acme - the nodes' own ACME client, in the
// Controller's image - do req: nothing of the Controller's environment,
// the request on its standard input, its log to stderr.
func RunClient(binary string, req *acmewire.Request, timeout time.Duration, stderr io.Writer) (*acmewire.Result, error) {
	in, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary)
	cmd.Env = []string{}
	cmd.Stdin = bytes.NewReader(in)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = stderr
	runErr := cmd.Run()
	res := &acmewire.Result{}
	if err := json.Unmarshal(out.Bytes(), res); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("janus-acme took over %s", timeout)
		}
		if runErr != nil {
			return nil, fmt.Errorf("janus-acme: %w", runErr)
		}
		return nil, fmt.Errorf("janus-acme's answer: %w", err)
	}
	if res.Error != "" {
		return res, errors.New(res.Error)
	}
	return res, nil
}
