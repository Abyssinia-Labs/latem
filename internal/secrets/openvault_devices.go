package secrets

import (
	"context"
	"encoding/hex"
	"fmt"
)

// OpenVaultDevice is one device of the vault, as the service lists it.
type OpenVaultDevice struct {
	// ID is the device's id in hex; its start names it to the CLI.
	ID       string
	Name     string
	Platform string
	This     bool
	Revoked  bool
}

// Device flags (OpenVault service.md, device flags).
const (
	ovThisDevice byte = 1 << 0
	ovRevoked    byte = 1 << 1
)

// Devices lists the vault's devices: names and ids, never a key.
func (p *OpenVaultProvider) Devices(ctx context.Context) ([]OpenVaultDevice, error) {
	typ, body, err := p.request(ctx, ovDevices)
	if err != nil {
		return nil, err
	}
	switch typ {
	case ovDeviceList:
		r := ovReader{b: body}
		n := r.u32()
		var out []OpenVaultDevice
		for i := uint32(0); i < n && r.err == nil; i++ {
			id := r.take(16)
			flags := r.u8()
			d := OpenVaultDevice{ID: hex.EncodeToString(id), Platform: r.str(), Name: r.str()}
			d.This, d.Revoked = flags&ovThisDevice != 0, flags&ovRevoked != 0
			out = append(out, d)
		}
		if r.err != nil || !r.done() {
			return nil, p.protocolError("devices", "a list of devices that does not parse")
		}
		return out, nil
	case ovError:
		return nil, p.serviceError("devices", body)
	}
	return nil, p.protocolError("devices", fmt.Sprintf("message type 0x%02x", typ))
}

// ThisDevice is this device as `openvault scope` takes it: its name, or the
// start of its id when another device of the vault has the same name, as a
// revoked one can, or when its name could not be read.
func (p *OpenVaultProvider) ThisDevice(ctx context.Context) (string, error) {
	devices, err := p.Devices(ctx)
	if err != nil {
		return "", err
	}
	return thisDevice(devices), nil
}

func thisDevice(devices []OpenVaultDevice) string {
	for _, d := range devices {
		if !d.This {
			continue
		}
		shared := d.Name == ""
		for _, o := range devices {
			shared = shared || (o.ID != d.ID && o.Name == d.Name)
		}
		if shared {
			return d.ID[:min(8, len(d.ID))]
		}
		return d.Name
	}
	return ""
}
