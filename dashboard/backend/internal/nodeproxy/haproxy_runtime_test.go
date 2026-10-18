package nodeproxy

import "testing"

func TestQueryFilters(t *testing.T) {
	got, err := queryFilters([]string{"http_req_rate:gt:100", "gpc(0):eq:-1"})
	if err != nil || len(got) != 2 || got[0].GetData() != "http_req_rate" || got[0].GetOp() != "gt" || got[0].GetValue() != 100 || got[1].GetValue() != -1 {
		t.Fatalf("filters = %v, %v", got, err)
	}
	for _, bad := range []string{"http_req_rate:gt", "a:b:c:d", "gpc0:eq:1.5", "gpc0:eq:"} {
		if _, err := queryFilters([]string{bad}); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
