package qqd

import (
	"fmt"
)

// ContainerRuntime captures the Podman/Quadlet operations the deploy pipeline
// needs behind a testable interface.
type ContainerRuntime interface {
	// Name returns the runtime identifier.
	Name() string

	// Cmd returns the CLI binary name.
	Cmd() string

	// ImageExistsCmd returns the shell command to check if an image exists (exit 0 = exists).
	ImageExistsCmd(image string) string

	// UnitDir returns the systemd unit directory path on the target.
	UnitDir() string

	// UnitExtension returns the file extension for generated unit files.
	UnitExtension() string

	// SystemctlPrefix returns the systemctl invocation prefix.
	SystemctlPrefix() string

	// RenderContainer renders a container unit file for a non-replicated service.
	RenderContainer(project, service string, cfg ServiceConfig) string

	// RenderContainerWithSlot renders a container unit file with a slot suffix for zero-downtime deploys.
	RenderContainerWithSlot(project, service, slot string, cfg ServiceConfig) string

	// RenderReplicaContainer renders a container unit file for a replica.
	RenderReplicaContainer(project, service string, replica int, cfg ServiceConfig) string

	// RenderNetwork renders the network unit file.
	RenderNetwork(project string) string

	// ContainerFileName returns the unit filename for a service.
	ContainerFileName(project, service string) string

	// ReplicaFileName returns the unit filename for a replica.
	ReplicaFileName(project, service string, replica int) string

	// NetworkFileName returns the network unit filename.
	NetworkFileName(project string) string
}

// PodmanRuntime implements ContainerRuntime using Podman + Quadlet.
type PodmanRuntime struct{}

func (PodmanRuntime) Name() string            { return "podman" }
func (PodmanRuntime) Cmd() string             { return "podman" }
func (PodmanRuntime) UnitDir() string         { return "~/.config/containers/systemd" }
func (PodmanRuntime) UnitExtension() string   { return ".container" }
func (PodmanRuntime) SystemctlPrefix() string { return "systemctl --user" }
func (PodmanRuntime) ContainerFileName(p, s string) string {
	return fmt.Sprintf("%s-%s.container", p, s)
}
func (PodmanRuntime) ReplicaFileName(p, s string, i int) string {
	return fmt.Sprintf("%s-%s-%d.container", p, s, i)
}
func (PodmanRuntime) NetworkFileName(p string) string { return fmt.Sprintf("%s.network", p) }

func (PodmanRuntime) ImageExistsCmd(image string) string {
	return fmt.Sprintf("podman image exists %s", shellQuote(image))
}

func (PodmanRuntime) RenderContainer(project, service string, cfg ServiceConfig) string {
	return renderContainer(project, service, cfg)
}

func (PodmanRuntime) RenderContainerWithSlot(project, service, slot string, cfg ServiceConfig) string {
	return renderContainerWithSlot(project, service, slot, cfg)
}

func (PodmanRuntime) RenderReplicaContainer(project, service string, replica int, cfg ServiceConfig) string {
	return renderReplicaContainer(project, service, replica, cfg)
}

func (PodmanRuntime) RenderNetwork(project string) string {
	return renderNetwork(project)
}

// runtimeByName returns the configured ContainerRuntime. Config validation
// rejects non-Podman values before this is called.
func runtimeByName(_ string) ContainerRuntime {
	return PodmanRuntime{}
}
