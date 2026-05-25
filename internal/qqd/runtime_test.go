package qqd

import (
	"io"
	"strings"
	"testing"
)

func TestPodmanRuntimeDefaults(t *testing.T) {
	rt := PodmanRuntime{}

	if rt.Name() != "podman" {
		t.Fatalf("expected podman, got %s", rt.Name())
	}
	if rt.Cmd() != "podman" {
		t.Fatalf("expected podman, got %s", rt.Cmd())
	}
	if rt.UnitExtension() != ".container" {
		t.Fatalf("expected .container, got %s", rt.UnitExtension())
	}
	if !strings.Contains(rt.UnitDir(), "containers/systemd") {
		t.Fatalf("expected containers/systemd path, got %s", rt.UnitDir())
	}
	if rt.ContainerFileName("proj", "web") != "proj-web.container" {
		t.Fatalf("unexpected filename: %s", rt.ContainerFileName("proj", "web"))
	}
	if rt.ReplicaFileName("proj", "web", 2) != "proj-web-2.container" {
		t.Fatalf("unexpected filename: %s", rt.ReplicaFileName("proj", "web", 2))
	}
	if rt.NetworkFileName("proj") != "proj.network" {
		t.Fatalf("unexpected filename: %s", rt.NetworkFileName("proj"))
	}
	if !strings.Contains(rt.ImageExistsCmd("nginx:1.25"), "podman image exists") {
		t.Fatalf("unexpected cmd: %s", rt.ImageExistsCmd("nginx:1.25"))
	}
}

func TestPodmanRenderMatchesExisting(t *testing.T) {
	rt := PodmanRuntime{}
	cfg := ServiceConfig{
		Image: "nginx:1.25",
		Env:   map[string]string{"PORT": "8080"},
	}

	got := rt.RenderContainer("proj", "web", cfg)
	expected := renderContainer("proj", "web", cfg)
	if got != expected {
		t.Fatalf("PodmanRuntime should delegate to renderContainer:\ngot:\n%s\nexpected:\n%s", got, expected)
	}
}

func TestRuntimeByName(t *testing.T) {
	tests := []struct {
		name     string
		wantName string
	}{
		{"", "podman"},
		{"podman", "podman"},
		{"Podman", "podman"},
	}
	for _, tt := range tests {
		rt := runtimeByName(tt.name)
		if rt.Name() != tt.wantName {
			t.Fatalf("runtimeByName(%q) = %s, want %s", tt.name, rt.Name(), tt.wantName)
		}
	}
}

func TestRuntimeFromConfig(t *testing.T) {
	app := &App{Stdout: io.Discard}

	// Default runtime
	if app.rt().Name() != "podman" {
		t.Fatal("default runtime should be podman")
	}

	app.applyConfig(ProjectConfig{Name: "test", Runtime: "podman"})
	if app.rt().Name() != "podman" {
		t.Fatal("runtime should remain podman after applyConfig")
	}
}

func TestDockerRuntimeConfigRejected(t *testing.T) {
	raw := map[string]any{
		"name":    "proj",
		"runtime": "docker",
		"sync":    "upload",
		"services": map[string]any{
			"web": map[string]any{"image": "nginx:1.25"},
		},
		"targets": map[string]any{
			"main": map[string]any{"host": "local", "repo_dir": "/repo"},
		},
	}
	_, err := decodeProjectConfig(raw, ".")
	if err == nil {
		t.Fatal("runtime docker should be rejected; qqd deploys with Podman only")
	}
	if !strings.Contains(err.Error(), "no longer supported") {
		t.Fatalf("rejection message should explain that runtime docker is dropped; got: %v", err)
	}
}

func TestRenderQuadletFilesWithPodmanRuntime(t *testing.T) {
	services := map[string]ServiceConfig{
		"web": {Image: "nginx:1.25"},
		"db":  {Image: "postgres:16", Replicas: 2},
	}
	expose := ExposeConfig{}

	files := renderQuadletFiles("proj", services, services, expose, TraefikProvider{}, PodmanRuntime{}, "deploy")

	wantNames := []string{
		"proj.network",
		"proj-web.container",
		"proj-db-1.container",
		"proj-db-2.container",
	}
	nameSet := map[string]bool{}
	for _, f := range files {
		nameSet[f.Name] = true
	}
	for _, want := range wantNames {
		if !nameSet[want] {
			t.Fatalf("missing file %s, got: %v", want, nameSet)
		}
	}
}
