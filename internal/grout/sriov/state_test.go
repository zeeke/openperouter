// SPDX-License-Identifier:Apache-2.0

package sriov

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSaveAndLoadVFState(t *testing.T) {
	dir := t.TempDir()
	state := VFState{
		InitialLinkName: "enp3s0f0v0",
		PCIAddress:      "0000:03:10.0",
		InitialDriver:   "iavf",
	}

	if err := SaveVFState(dir, "enp3s0f0v0", state); err != nil {
		t.Fatalf("SaveVFState failed: %v", err)
	}

	got, err := LoadVFState(dir, "enp3s0f0v0")
	if err != nil {
		t.Fatalf("LoadVFState failed: %v", err)
	}
	if got == nil {
		t.Fatal("LoadVFState returned nil")
	}
	if *got != state {
		t.Errorf("loaded state %+v does not match saved %+v", *got, state)
	}
}

func TestLoadVFStateNotFound(t *testing.T) {
	dir := t.TempDir()

	got, err := LoadVFState(dir, "nonexistent")
	if err != nil {
		t.Fatalf("LoadVFState should not error on missing file: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for missing state file, got %+v", *got)
	}
}

func TestRemoveVFState(t *testing.T) {
	dir := t.TempDir()
	state := VFState{
		InitialLinkName: "enp3s0f0v0",
		PCIAddress:      "0000:03:10.0",
		InitialDriver:   "iavf",
	}

	if err := SaveVFState(dir, "enp3s0f0v0", state); err != nil {
		t.Fatalf("SaveVFState failed: %v", err)
	}

	if err := RemoveVFState(dir, "enp3s0f0v0"); err != nil {
		t.Fatalf("RemoveVFState failed: %v", err)
	}

	path := filepath.Join(dir, "vf_enp3s0f0v0")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("state file should have been removed, stat err: %v", err)
	}
}

func TestRemoveVFStateNotFound(t *testing.T) {
	dir := t.TempDir()

	if err := RemoveVFState(dir, "nonexistent"); err != nil {
		t.Fatalf("RemoveVFState should not error on missing file: %v", err)
	}
}

func TestSaveVFStateCreatesDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "dir")
	state := VFState{
		InitialLinkName: "enp3s0f0v0",
		PCIAddress:      "0000:03:10.0",
		InitialDriver:   "iavf",
	}

	if err := SaveVFState(dir, "enp3s0f0v0", state); err != nil {
		t.Fatalf("SaveVFState should create missing directories: %v", err)
	}

	got, err := LoadVFState(dir, "enp3s0f0v0")
	if err != nil {
		t.Fatalf("LoadVFState failed: %v", err)
	}
	if got == nil || *got != state {
		t.Errorf("round-trip through nested dir failed: got %+v", got)
	}
}
