package main

import (
	"reflect"
	"testing"
)

func TestParsePnpNameservers(t *testing.T) {
	cases := []struct {
		name string
		pnp  string
		want []string
	}{
		{
			name: "typical kernel IP_PNP output",
			pnp: "#PROTO: DHCP\n" +
				"domain example.com\n" +
				"nameserver 172.16.1.1\n" +
				"nameserver 172.16.1.2\n",
			want: []string{"172.16.1.1", "172.16.1.2"},
		},
		{
			name: "single nameserver, no domain line",
			pnp:  "#PROTO: DHCP\nnameserver 10.1.0.1\n",
			want: []string{"10.1.0.1"},
		},
		{
			name: "no nameserver at all",
			pnp:  "#PROTO: DHCP\ndomain example.com\n",
			want: nil,
		},
		{
			name: "empty file",
			pnp:  "",
			want: nil,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parsePnpNameservers([]byte(c.pnp))
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("parsePnpNameservers(%q) = %v, want %v", c.pnp, got, c.want)
			}
		})
	}
}
