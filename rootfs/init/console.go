package main

import (
	"io"
	"os"
	"syscall"
)

// screenDevice is the kernel's screen console - the framebuffer on a
// machine with a display. A var so tests can point it elsewhere.
var screenDevice = "/dev/tty0"

// screenQueue bounds what waits for the screen: past it, lines are
// dropped from the screen, never from the serial console.
const screenQueue = 256

// mirrorConsole sends what init and janusd write from now on to the
// screen as well as to /dev/console. The kernel's console= list puts
// /dev/console on the serial port (the last console=), and a process's
// output only reaches /dev/console - an operator in front of a server's
// monitor would see the kernel boot, then nothing: not the motd, not the
// first boot's credentials.
//
// os.Stdout and os.Stderr - what init prints with and hands janusd -
// become a pipe that one goroutine copies, for init's whole life, to the
// serial console exactly as before (synchronously) and to the screen
// best effort: a slow or missing screen drops lines rather than ever
// holding janusd back. File descriptors 1 and 2 themselves stay on
// /dev/console, so a Go runtime crash of init still reaches it.
// Without a screen console (no /dev/tty0), nothing changes.
func mirrorConsole() {
	screen, err := os.OpenFile(screenDevice, os.O_WRONLY|syscall.O_NOCTTY, 0)
	if err != nil {
		return
	}
	r, w, err := os.Pipe()
	if err != nil {
		screen.Close()
		return
	}
	serial := os.Stdout
	toScreen := make(chan []byte, screenQueue)
	go func() {
		for b := range toScreen {
			_, _ = screen.Write(b)
		}
	}()
	go relayConsole(r, serial, toScreen)
	os.Stdout, os.Stderr = w, w
}

// relayConsole copies r to serial, and hands each chunk to toScreen
// without waiting for it.
func relayConsole(r io.Reader, serial io.Writer, toScreen chan<- []byte) {
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			_, _ = serial.Write(buf[:n])
			chunk := append([]byte(nil), buf[:n]...)
			select {
			case toScreen <- chunk:
			default: // the screen is behind: it misses this chunk
			}
		}
		if err != nil {
			return
		}
	}
}
