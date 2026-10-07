package main

import (
	"log"

	"github.com/swenske/Janus/internal/boottime"
)

// measureBoot completes this boot's timing (internal/boottime: init
// wrote its two moments) when this is the boot's first janusd, logs it
// and saves it; a janusd restarted later finds it complete - its own
// start isn't the boot's. haproxyServingAt is the uptime at which
// HAProxy's Boot returned. Nil without init's file (not a node boot).
func measureBoot(haproxyServingAt float64) *boottime.Times {
	t, err := boottime.Load(boottime.File)
	if err != nil {
		log.Printf("boot: %v", err)
		return nil
	}
	if t == nil || t.API != 0 {
		return t
	}
	api, err := boottime.Uptime()
	if err != nil {
		log.Printf("boot: %v", err)
		return nil
	}
	t.HAProxy, t.API = haproxyServingAt, api
	log.Printf("boot: %s", t)
	if err := t.Save(boottime.File); err != nil {
		log.Printf("boot: save: %v", err)
	}
	return t
}
