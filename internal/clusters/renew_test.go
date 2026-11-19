package clusters

import (
	"os"
	"regexp"
	"strconv"
	"testing"
	"time"
)

// A member certificate is renewed before the example
// JanusCertificateExpiresSoon alert fires, with a day of the reconcile's
// retries to spare: the alert then means a renewal failed. Renewed at
// two thirds of its 30 days (10 left), every cluster member raised the
// 14-day alert for four days each cycle.
func TestMemberRenewedBeforeTheAlert(t *testing.T) {
	data, err := os.ReadFile("../../examples/prometheus/janus-rules.yml")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`alert: JanusCertificateExpiresSoon\s+expr: janus_certificate_expiry_timestamp_seconds - time\(\) < (\d+) \* 86400\n`).FindSubmatch(data)
	if m == nil {
		t.Fatal("JanusCertificateExpiresSoon's expression not found in examples/prometheus/janus-rules.yml: update this test with it")
	}
	days, err := strconv.Atoi(string(m[1]))
	if err != nil {
		t.Fatal(err)
	}
	alert := time.Duration(days) * 24 * time.Hour
	if MemberRenewBefore-alert < 24*time.Hour {
		t.Errorf("member certificates renewed with %v left, the alert fires at %v: less than a day before it", MemberRenewBefore, alert)
	}
	if MemberRenewBefore >= MemberValidity {
		t.Errorf("renewed with %v left of %v: at every check", MemberRenewBefore, MemberValidity)
	}
}
