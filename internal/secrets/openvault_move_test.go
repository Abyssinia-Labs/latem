package secrets

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// listenVaultAt is a fake vault service on socket, a path the test chose,
// where newFakeVault picks its own. close stops it and removes the socket, as
// a stopped service does.
func listenVaultAt(t *testing.T, socket string, answer func(r ovRequest) []byte) (close func()) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(socket), 0o700); err != nil {
		t.Fatal(err)
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	v := &fakeVault{t: t, ln: ln, socket: socket, answer: answer}
	go v.serve()
	var once sync.Once
	close = func() { once.Do(func() { _ = ln.Close() }) }
	t.Cleanup(close)
	return close
}

// movingHome is a home directory short enough for a socket under it, with
// the vault's command on its PATH as pinkvault.
func movingHome(t *testing.T) (home string, environ []string) {
	t.Helper()
	home, err := os.MkdirTemp("", "ovh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	bin := filepath.Join(home, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, VaultCommand), []byte("#!/bin/sh\n"), 0o700); err != nil { // #nosec G306 -- a fake command
		t.Fatal(err)
	}
	return home, []string{"HOME=" + home, "PATH=" + bin}
}

// moveVault stops the service at ~/.latem-vault, moves the folder to
// ~/.pinkvault and starts a service there answering value, as PinkVault
// 0.7.0's `service install` does.
func moveVault(t *testing.T, home string, stopOld func(), value string) string {
	t.Helper()
	stopOld()
	if err := os.Rename(filepath.Join(home, latemVaultHomeDir), filepath.Join(home, vaultHomeDir)); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(home, vaultHomeDir, OpenVaultSocketName)
	listenVaultAt(t, socket, vaultOf(map[string]string{"latem-cloud/production/TOKEN": value}))
	return socket
}

func TestOpenVault_AVaultThatMovesIsFoundAgainWithoutARestart(t *testing.T) {
	home, environ := movingHome(t)
	oldSocket := filepath.Join(home, latemVaultHomeDir, OpenVaultSocketName)
	stopOld := listenVaultAt(t, oldSocket, vaultOf(map[string]string{"latem-cloud/production/TOKEN": "before"}))

	// What the agent does at start: find the socket once.
	socket, err := OpenVaultSocket(environ)
	if err != nil {
		t.Fatal(err)
	}
	if socket != oldSocket {
		t.Fatalf("socket = %s, want %s", socket, oldSocket)
	}
	p := NewOpenVaultProvider(socket, 0).WithCommand(LatemVaultCommand).WithEnviron(environ)
	got, err := p.ResolveIn(context.Background(), []string{"latem-cloud", "production"}, "TOKEN")
	if err != nil || string(got) != "before" {
		t.Fatalf("before the move: %q, %v", got, err)
	}

	newSocket := moveVault(t, home, stopOld, "after")

	got, err = p.ResolveIn(context.Background(), []string{"latem-cloud", "production"}, "TOKEN")
	if err != nil {
		t.Fatalf("after the move: %v, want the moved vault's answer", err)
	}
	if string(got) != "after" {
		t.Errorf("value = %q, want the moved vault's", got)
	}
	if p.Socket() != newSocket {
		t.Errorf("Socket() = %s, want %s", p.Socket(), newSocket)
	}
	if p.Command() != VaultCommand {
		t.Errorf("Command() = %q, want %q, the command a person now types", p.Command(), VaultCommand)
	}
}

func TestOpenVault_AStoppedVaultIsStillNotRunning(t *testing.T) {
	t.Run("not moved", func(t *testing.T) {
		// Looking again finds the same socket, so a stopped service is what
		// it was: a backend that is down.
		home, environ := movingHome(t)
		socket := filepath.Join(home, latemVaultHomeDir, OpenVaultSocketName)
		stop := listenVaultAt(t, socket, vaultOf(nil))
		p := NewOpenVaultProvider(socket, 0).WithEnviron(environ)
		stop()
		_, err := p.ResolveIn(context.Background(), []string{"latem-cloud"}, "TOKEN")
		var oe *OpenVaultError
		if !errors.As(err, &oe) || !oe.NotRunning {
			t.Fatalf("err = %v, want a not-running OpenVaultError", err)
		}
		if p.Socket() != socket {
			t.Errorf("Socket() = %s, want it unchanged", p.Socket())
		}
	})
	t.Run("moved, with nothing to look in", func(t *testing.T) {
		// A provider made without an environment keeps the socket it was
		// given, as before A114.
		home, _ := movingHome(t)
		socket := filepath.Join(home, latemVaultHomeDir, OpenVaultSocketName)
		stop := listenVaultAt(t, socket, vaultOf(nil))
		p := NewOpenVaultProvider(socket, 0)
		moveVault(t, home, stop, "after")
		_, err := p.ResolveIn(context.Background(), []string{"latem-cloud", "production"}, "TOKEN")
		var oe *OpenVaultError
		if !errors.As(err, &oe) || !oe.NotRunning {
			t.Fatalf("err = %v, want a not-running OpenVaultError", err)
		}
	})
	t.Run("moved, with its service not started yet", func(t *testing.T) {
		// The folder moved and its socket is not there yet: not running,
		// and the provider stays put until a socket is.
		home, environ := movingHome(t)
		socket := filepath.Join(home, latemVaultHomeDir, OpenVaultSocketName)
		stop := listenVaultAt(t, socket, vaultOf(nil))
		p := NewOpenVaultProvider(socket, 0).WithEnviron(environ)
		stop()
		if err := os.Rename(filepath.Join(home, latemVaultHomeDir), filepath.Join(home, vaultHomeDir)); err != nil {
			t.Fatal(err)
		}
		_, err := p.ResolveIn(context.Background(), []string{"latem-cloud"}, "TOKEN")
		var oe *OpenVaultError
		if !errors.As(err, &oe) || !oe.NotRunning {
			t.Fatalf("err = %v, want a not-running OpenVaultError", err)
		}
		if p.Socket() != socket {
			t.Errorf("Socket() = %s, want it unchanged until the new service is up", p.Socket())
		}
	})
}

func TestOpenVault_AMoveIsFollowedOnceUnderConcurrentReads(t *testing.T) {
	home, environ := movingHome(t)
	oldSocket := filepath.Join(home, latemVaultHomeDir, OpenVaultSocketName)
	stopOld := listenVaultAt(t, oldSocket, vaultOf(nil))
	p := NewOpenVaultProvider(oldSocket, 0).WithEnviron(environ)
	newSocket := moveVault(t, home, stopOld, "after")

	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := p.ResolveIn(context.Background(), []string{"latem-cloud", "production"}, "TOKEN")
			if err == nil && string(got) != "after" {
				err = errors.New("value " + string(got))
			}
			if err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if p.Socket() != newSocket {
		t.Errorf("Socket() = %s, want %s", p.Socket(), newSocket)
	}
}
