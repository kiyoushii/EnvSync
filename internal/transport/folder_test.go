package transport

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestApplyFolderSharesWithAllPeersAndIsIdempotent(t *testing.T) {
	c, f := newFake(t)
	dir := filepath.Join(t.TempDir(), "Projects")
	spec := FolderSpec{ID: "projects", Path: dir, KeepDays: 60, WatcherDelayS: 3, MaxConflicts: 10, Ignores: []string{"(?i)node_modules"}}

	for i := 0; i < 2; i++ { // повтор не должен плодить устройства
		if err := ApplyFolder(context.Background(), c, selfID, []string{peerID, "BBBBBBB-CCCCCCC-DDDDDDD-EEEEEEE-FFFFFFF-GGGGGGG-HHHHHHH-AAAAAAA"}, spec); err != nil {
			t.Fatal(err)
		}
	}
	folder := f.folders["projects"]
	if devs, _ := folder["devices"].([]any); len(devs) != 3 {
		t.Errorf("ожидалось 3 устройства (мы + 2 соседа), есть %v", devs)
	}
	v, _ := folder["versioning"].(map[string]any)
	params, _ := v["params"].(map[string]any)
	if v["type"] != "trashcan" || params["cleanoutDays"] != "60" {
		t.Errorf("versioning = %v", v)
	}
	if len(f.ignores) != 1 || f.ignores[0] != "(?i)node_modules" {
		t.Errorf("ignores = %v", f.ignores)
	}
}

// Защита данных: существующую папку нельзя молча перенацелить на другой путь.
func TestApplyFolderRefusesToRetargetExistingFolder(t *testing.T) {
	c, f := newFake(t)
	base := t.TempDir()
	first := FolderSpec{ID: "projects", Path: filepath.Join(base, "Old")}
	if err := ApplyFolder(context.Background(), c, selfID, []string{peerID}, first); err != nil {
		t.Fatal(err)
	}

	err := ApplyFolder(context.Background(), c, selfID, []string{peerID}, FolderSpec{ID: "projects", Path: filepath.Join(base, "New")})
	if err == nil || !strings.Contains(err.Error(), "небезопасно") {
		t.Fatalf("ожидали отказ, получили %v", err)
	}
	if got := f.folders["projects"]["path"]; got != first.Path {
		t.Errorf("путь папки изменился: %v", got)
	}
	// Тот же путь в другом регистре и с лишним слэшем — не повод для отказа.
	same := FolderSpec{ID: "projects", Path: strings.ToUpper(first.Path) + `\`}
	if err := ApplyFolder(context.Background(), c, selfID, []string{peerID}, same); err != nil {
		t.Fatalf("тот же путь должен приниматься: %v", err)
	}
}
