package api

import (
	"encoding/binary"
	"testing"
	"time"
)

func TestCompilePcapFilter(t *testing.T) {
	if prog, err := compilePcapFilter(""); err != nil || prog != nil {
		t.Errorf(`compilePcapFilter("") = %v, %v; want no filter`, prog, err)
	}
	if prog, err := compilePcapFilter("tcp port 8080"); err != nil || len(prog) == 0 {
		t.Errorf("compilePcapFilter(tcp port 8080) = %d instructions, %v", len(prog), err)
	}
	if _, err := compilePcapFilter("definitely not a filter"); err == nil {
		t.Error("garbage expression compiled without error")
	}
}

func TestPcapFraming(t *testing.T) {
	h := pcapGlobalHeader(1500, pcapLinkTypeEther)
	if len(h) != 24 || binary.LittleEndian.Uint32(h) != 0xa1b2c3d4 ||
		binary.LittleEndian.Uint16(h[4:]) != 2 || binary.LittleEndian.Uint16(h[6:]) != 4 ||
		binary.LittleEndian.Uint32(h[16:]) != 1500 || binary.LittleEndian.Uint32(h[20:]) != 1 {
		t.Fatalf("bad global header: % x", h)
	}

	ts := time.Unix(1790000000, 123456789)
	rec := appendPcapRecord(nil, ts, []byte{1, 2, 3}, 60)
	if len(rec) != 19 ||
		binary.LittleEndian.Uint32(rec[0:]) != 1790000000 ||
		binary.LittleEndian.Uint32(rec[4:]) != 123456 ||
		binary.LittleEndian.Uint32(rec[8:]) != 3 ||
		binary.LittleEndian.Uint32(rec[12:]) != 60 {
		t.Fatalf("bad record: % x", rec)
	}

	if htons(0x0003) != 0x0300 {
		t.Errorf("htons(0x0003) = %#x", htons(0x0003))
	}
}
