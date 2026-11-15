package secrets

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"
)

func ovDeviceListOf(devices ...OpenVaultDevice) []byte {
	b := binary.BigEndian.AppendUint32([]byte{ovDeviceList}, uint32(len(devices)))
	for i, d := range devices {
		b = append(b, bytes.Repeat([]byte{byte(i + 1)}, 16)...)
		var flags byte
		if d.This {
			flags |= ovThisDevice
		}
		if d.Revoked {
			flags |= ovRevoked
		}
		b = append(b, flags)
		b = append(b, ovStr(d.Platform)...)
		b = append(b, ovStr(d.Name)...)
	}
	return b
}

// LAT-16: setup names this device as `openvault scope` takes it, read from
// the service, and by the start of its id when its name is not its own alone.
func TestOpenVaultThisDevice_IsItsNameOrTheStartOfItsIdWhenTheNameIsShared(t *testing.T) {
	for _, c := range []struct {
		name    string
		devices []OpenVaultDevice
		want    string
	}{
		{"its own name", []OpenVaultDevice{{Name: "MacBook", This: true}, {Name: "Mini"}}, "MacBook"},
		{"a name a revoked device had", []OpenVaultDevice{{Name: "Desktop", Revoked: true}, {Name: "Desktop", This: true}}, "02020202"},
		{"a name that did not open", []OpenVaultDevice{{Name: "", This: true}}, "01010101"},
	} {
		t.Run(c.name, func(t *testing.T) {
			v := newFakeVault(t, func(r ovRequest) []byte {
				if r.typ != ovDevices {
					return ovRefusal("unexpected request")
				}
				return ovDeviceListOf(c.devices...)
			})
			got, err := NewOpenVaultProvider(v.socket, 0).ThisDevice(context.Background())
			if err != nil || got != c.want {
				t.Errorf("ThisDevice = %q, %v; want %q", got, err, c.want)
			}
		})
	}
}
