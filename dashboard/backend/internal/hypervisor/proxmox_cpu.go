package hypervisor

import (
	"fmt"
	"slices"
)

// The CPU model a Proxmox VE hypervisor's machines get (ProxmoxConfig.
// CPUModel, #372). Proxmox's API gives kvm64 when it's not set: no
// AES-NI, SSE4, AVX, BMI2 or ADX - HAProxy's AWS-LC fell back to ChaCha20
// and generic code for every curve and hash.
//
// The models a hypervisor's setting may name, from qemu-server's
// CPUConfig.pm (Proxmox VE 9): its x86-64 levels with AES, the host's own
// CPU, and the named QEMU models with AES-NI (Westmere and Opteron_G4
// on). Left out: those without AES-NI (kvm64, qemu64, x86-64-v2, the
// older named ones), 32-bit ones, max, custom models (the token would
// need Mapping.Use on each: not in the host's preparation).
//
// Every one of them is a model the Controller may give: a machine whose
// model is one of them, other than its hypervisor's, shows the difference
// (the Proxmox driver's cpuDrift); any other model is someone's choice,
// left alone.

// DefaultProxmoxCPUModel is the model of a hypervisor that names none:
// what Proxmox's own form gives a new machine. It runs on any x86-64-v2
// host with AES, so a machine migrates live between a cluster's hosts.
const DefaultProxmoxCPUModel = "x86-64-v2-AES"

// ProxmoxCPULevels are the x86-64 levels the setting takes, oldest
// first, with "host" - what the Controller's form offers first.
var ProxmoxCPULevels = []string{DefaultProxmoxCPUModel, "x86-64-v3", "x86-64-v4", "host"}

// proxmoxNamedCPUModels are QEMU's named x86-64 models with AES-NI, as
// qemu-server lists them.
var proxmoxNamedCPUModels = []string{
	// Intel
	"Westmere", "Westmere-IBRS",
	"SandyBridge", "SandyBridge-IBRS", "IvyBridge", "IvyBridge-IBRS",
	"Haswell", "Haswell-IBRS", "Haswell-noTSX", "Haswell-noTSX-IBRS",
	"Broadwell", "Broadwell-IBRS", "Broadwell-noTSX", "Broadwell-noTSX-IBRS",
	"Skylake-Client", "Skylake-Client-IBRS", "Skylake-Client-noTSX-IBRS", "Skylake-Client-v4",
	"Skylake-Server", "Skylake-Server-IBRS", "Skylake-Server-noTSX-IBRS", "Skylake-Server-v4", "Skylake-Server-v5",
	"Cascadelake-Server", "Cascadelake-Server-v2", "Cascadelake-Server-noTSX", "Cascadelake-Server-v4", "Cascadelake-Server-v5",
	"Cooperlake", "Cooperlake-v2",
	"Icelake-Server", "Icelake-Server-noTSX", "Icelake-Server-v3", "Icelake-Server-v4", "Icelake-Server-v5", "Icelake-Server-v6", "Icelake-Server-v7",
	"SapphireRapids", "SapphireRapids-v2", "SapphireRapids-v3", "SapphireRapids-v4",
	"GraniteRapids", "GraniteRapids-v2", "GraniteRapids-v3",
	"SierraForest", "SierraForest-v2", "SierraForest-v3", "ClearwaterForest",
	// AMD
	"Opteron_G4", "Opteron_G5",
	"EPYC", "EPYC-IBPB", "EPYC-v3", "EPYC-v4", "EPYC-v5",
	"EPYC-Rome", "EPYC-Rome-v2", "EPYC-Rome-v3", "EPYC-Rome-v4", "EPYC-Rome-v5",
	"EPYC-Milan", "EPYC-Milan-v2", "EPYC-Milan-v3",
	"EPYC-Genoa", "EPYC-Genoa-v2", "EPYC-Turin",
}

// IsProxmoxCPUModel reports whether a hypervisor's setting may name
// model - and so whether a machine with it has a model the Controller
// gives.
func IsProxmoxCPUModel(model string) bool {
	return slices.Contains(ProxmoxCPULevels, model) || slices.Contains(proxmoxNamedCPUModels, model)
}

// CPU is the CPU model the hypervisor's machines get.
func (c *ProxmoxConfig) CPU() string {
	if c.CPUModel != "" {
		return c.CPUModel
	}
	return DefaultProxmoxCPUModel
}

func checkProxmoxCPUModel(model string) error {
	if model == "" || IsProxmoxCPUModel(model) {
		return nil
	}
	switch model {
	case "kvm64", "qemu64", "x86-64-v2":
		return fmt.Errorf("CPU model %s has no AES-NI: HAProxy's TLS would run on generic code - %s is the slowest model it may have", model, DefaultProxmoxCPUModel)
	}
	return fmt.Errorf("CPU model %q isn't one the Controller gives its machines: x86-64-v2-AES, x86-64-v3, x86-64-v4, host, or a named QEMU model with AES-NI (Haswell-noTSX, Skylake-Client-v4, EPYC-Rome...), as Proxmox VE writes it", model)
}
