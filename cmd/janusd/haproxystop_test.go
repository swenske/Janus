package main

import (
	"net"
	"os"
	"testing"
)

// This process stands for HAProxy: a client's connection to one of its
// listeners holds a stop; its own connection to a server, and one to its
// peers' listener, don't.
func TestClientsWatch(t *testing.T) {
	listen := func() net.Listener {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { l.Close() })
		return l
	}
	frontend, peers := listen(), listen()
	peersPort := peers.Addr().(*net.TCPAddr).Port
	watch := clientsWatch("/proc", os.Getpid(), peersPort)
	if watch == nil {
		t.Fatal("no watch over this process")
	}
	if watch() {
		t.Error("listeners alone are no client")
	}
	// A server elsewhere (another process would do; a listener opened
	// after the watch began isn't one of its ports).
	server := listen()
	toServer, err := net.Dial("tcp", server.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer toServer.Close()
	fromServer, _ := server.Accept()
	defer fromServer.Close()
	toPeers, err := net.Dial("tcp", peers.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer toPeers.Close()
	fromPeers, _ := peers.Accept()
	defer fromPeers.Close()
	if watch() {
		t.Error("a server's connection or a peer's held the stop")
	}
	client, err := net.Dial("tcp", frontend.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	accepted, _ := frontend.Accept()
	if !watch() {
		t.Error("a client's connection not seen")
	}
	client.Close()
	accepted.Close()
	if watch() {
		t.Error("a closed client's connection still seen")
	}
	if clientsWatch("/proc", -1, peersPort) != nil {
		t.Error("unreadable descriptors watched")
	}
}
