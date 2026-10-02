package hypervisor

import (
	"encoding/binary"
	"errors"
	"io"
)

// QCOW2VirtualSize reads the disk size a qcow2 file describes from its
// header - what a volume created to receive the file must say.
func QCOW2VirtualSize(r io.ReaderAt) (uint64, error) {
	var hdr [32]byte
	if _, err := r.ReadAt(hdr[:], 0); err != nil {
		return 0, err
	}
	if string(hdr[:4]) != "QFI\xfb" {
		return 0, errors.New("not a qcow2 file")
	}
	return binary.BigEndian.Uint64(hdr[24:32]), nil
}
