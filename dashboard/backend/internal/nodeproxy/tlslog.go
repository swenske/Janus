package nodeproxy

import (
	"context"
	"errors"
	"io"
	"regexp"
	"sort"
	"strings"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
)

// The TLS versions and ciphers of HAProxy's clients, for Metrics: no
// counter says them - only an access log line does (%sslv, %sslc;
// "option httpslog" ends each line with sni/version/cipher). tlsLog reads
// the last lines of HAProxy's output on the node (SystemService.Logs:
// this boot's, from memory - its access log when HAProxy logs to stdout)
// and counts them. A sample of what was logged, not every request: a
// configuration logging only errors gives theirs.

// tlsLogLines is how far back it reads: what a node keeps of a service.
const tlsLogLines = 5000

// tlsLogLimit bounds what it reads (5000 long lines).
const tlsLogLimit = 8 << 20

// tlsField finds a version and a cipher side by side - "TLSv1.3/
// TLS_AES_128_GCM_SHA256" (httpslog), or apart by a space as a
// log-format may put them.
var tlsField = regexp.MustCompile(`(?:^|[\s/|{])(TLSv1(?:\.[0-3])?|SSLv3)[/ ]([A-Z0-9][A-Z0-9_-]{5,})(?:$|[\s/|}])`)

// accessDate finds an access log line: HAProxy's date in brackets, as
// httplog, tcplog, httpslog and clf put it ([19/Oct/2026:10:12:01.123]).
var accessDate = regexp.MustCompile(`\[\d{2}/[A-Z][a-z]{2}/\d{4}:\d{2}:\d{2}:\d{2}`)

// NamedCount is a name and how many lines carried it.
type NamedCount struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// TLSLog is what the lines read say: how many there were, how many were
// access log lines, how many named a TLS version and a cipher, and those
// counted, the most first. No access line: HAProxy logs elsewhere, or
// not at all; access lines but no TLS: not with the version and cipher.
type TLSLog struct {
	Lines    int          `json:"lines"`
	Access   int          `json:"access"`
	TLS      int          `json:"tls"`
	Versions []NamedCount `json:"versions"`
	Ciphers  []NamedCount `json:"ciphers"`
}

// parseTLSLog counts the versions and ciphers in HAProxy's lines.
func parseTLSLog(text string) TLSLog {
	out := TLSLog{Versions: []NamedCount{}, Ciphers: []NamedCount{}}
	versions, ciphers := map[string]int{}, map[string]int{}
	for line := range strings.SplitSeq(text, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		out.Lines++
		m := tlsField.FindStringSubmatch(line)
		if m != nil || accessDate.MatchString(line) {
			out.Access++
		}
		if m == nil {
			continue
		}
		out.TLS++
		versions[m[1]]++
		ciphers[m[2]]++
	}
	out.Versions, out.Ciphers = ranked(versions), ranked(ciphers)
	return out
}

func ranked(m map[string]int) []NamedCount {
	out := make([]NamedCount, 0, len(m))
	for name, n := range m {
		out = append(out, NamedCount{name, n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// readTLSLog reads HAProxy's last lines on the node and counts them.
func readTLSLog(ctx context.Context, c janusv1alpha1.SystemServiceClient) (TLSLog, error) {
	stream, err := c.Logs(ctx, &janusv1alpha1.LogsRequest{Id: "haproxy", TailLines: tlsLogLines})
	if err != nil {
		return TLSLog{}, err
	}
	var b strings.Builder
	for b.Len() < tlsLogLimit {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return TLSLog{}, err
		}
		b.Write(msg.GetBytes())
	}
	return parseTLSLog(b.String()), nil
}
