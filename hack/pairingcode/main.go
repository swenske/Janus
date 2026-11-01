// Command pairingcode makes the codes hack/qemu-pairing-test.sh needs
// from a node's own (internal/pairing), each with a valid check - what a
// person could type, not a typo janusctl catches:
//
//	go run ./hack/pairingcode flip CODE      another certificate's code, the same secret
//	go run ./hack/pairingcode wrong CODE [N] the same certificate's, another secret
package main

import (
	"fmt"
	"log"
	"os"
	"strconv"

	"github.com/swenske/Janus/internal/pairing"
)

func main() {
	if len(os.Args) < 3 {
		log.Fatal("usage: pairingcode flip|wrong CODE [N]")
	}
	c, err := pairing.ParseCode(os.Args[2])
	if err != nil {
		log.Fatal(err)
	}
	n := uint64(1)
	if len(os.Args) > 3 {
		if n, err = strconv.ParseUint(os.Args[3], 10, 8); err != nil || n == 0 {
			log.Fatal("N: a number from 1")
		}
	}
	switch os.Args[1] {
	case "flip":
		c.Prefix ^= n
	case "wrong":
		c.Secret ^= n
	default:
		log.Fatal("usage: pairingcode flip|wrong CODE [N]")
	}
	fmt.Println(c)
}
