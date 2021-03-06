/*
Copyright 2016-2017 Mirantis

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package libvirttools

import (
	"encoding/json"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	kubeapi "k8s.io/kubernetes/pkg/kubelet/apis/cri/v1alpha1/runtime"
	kubetypes "k8s.io/kubernetes/pkg/kubelet/types"

	"github.com/Mirantis/virtlet/pkg/flexvolume"
	"github.com/Mirantis/virtlet/pkg/metadata"
	"github.com/Mirantis/virtlet/pkg/utils"
	testutils "github.com/Mirantis/virtlet/pkg/utils/testing"
	"github.com/Mirantis/virtlet/pkg/virt/fake"
	"github.com/Mirantis/virtlet/tests/criapi"
	"github.com/Mirantis/virtlet/tests/gm"
)

const (
	fakeImageName        = "fake/image1"
	fakeCNIConfig        = `{"noCniForNow":true}`
	fakeUUID             = "abb67e3c-71b3-4ddd-5505-8c4215d5c4eb"
	fakeContainerName    = "container1"
	fakeContainerAttempt = 42
	stopContainerTimeout = 30 * time.Second
)

type containerTester struct {
	t              *testing.T
	clock          clockwork.FakeClock
	tmpDir         string
	kubeletRootDir string
	virtTool       *VirtualizationTool
	rec            *testutils.TopLevelRecorder
	domainConn     *fake.FakeDomainConnection
	storageConn    *fake.FakeStorageConnection
	metadataStore  metadata.Store
}

func newContainerTester(t *testing.T, rec *testutils.TopLevelRecorder) *containerTester {
	ct := &containerTester{
		t:     t,
		clock: clockwork.NewFakeClockAt(time.Date(2017, 5, 30, 20, 19, 0, 0, time.UTC)),
	}

	var err error
	ct.tmpDir, err = ioutil.TempDir("", "virtualization-test-")
	if err != nil {
		t.Fatalf("TempDir(): %v", err)
	}

	// __config__  is a hint for fake libvirt domain to fix the path
	SetConfigIsoDir(filepath.Join(ct.tmpDir, "__config__"))

	ct.rec = rec
	ct.domainConn = fake.NewFakeDomainConnection(ct.rec.Child("domain conn"))
	ct.storageConn = fake.NewFakeStorageConnection(ct.rec.Child("storage"))

	ct.metadataStore, err = metadata.NewFakeStore()
	if err != nil {
		t.Fatalf("Failed to create fake bolt client: %v", err)
	}

	imageManager := NewFakeImageManager(ct.rec)
	ct.virtTool = NewVirtualizationTool(ct.domainConn, ct.storageConn, imageManager, ct.metadataStore, "volumes", "loop*", GetDefaultVolumeSource())
	ct.virtTool.SetClock(ct.clock)
	// avoid unneeded diffs in the golden master data
	ct.virtTool.SetForceKVM(true)
	ct.kubeletRootDir = filepath.Join(ct.tmpDir, "kubelet-root")
	ct.virtTool.SetKubeletRootDir(ct.kubeletRootDir)

	return ct
}

func (ct *containerTester) setPodSandbox(config *kubeapi.PodSandboxConfig) {
	psi, _ := metadata.NewPodSandboxInfo(config, nil, kubeapi.PodSandboxState_SANDBOX_READY, ct.clock)
	sandbox := ct.metadataStore.PodSandbox(config.Metadata.Uid)
	err := sandbox.Save(
		func(c *metadata.PodSandboxInfo) (*metadata.PodSandboxInfo, error) {
			return psi, nil
		},
	)
	if err != nil {
		ct.t.Fatalf("Failed to store pod sandbox: %v", err)
	}
}

func (ct *containerTester) teardown() {
	os.RemoveAll(ct.tmpDir)
}

func (ct *containerTester) createContainer(sandbox *kubeapi.PodSandboxConfig, mounts []*kubeapi.Mount) string {
	req := &kubeapi.CreateContainerRequest{
		PodSandboxId: sandbox.Metadata.Uid,
		Config: &kubeapi.ContainerConfig{
			Metadata: &kubeapi.ContainerMetadata{
				Name:    fakeContainerName,
				Attempt: fakeContainerAttempt,
			},
			Image: &kubeapi.ImageSpec{
				Image: fakeImageName,
			},
			Mounts:      mounts,
			Annotations: map[string]string{"foo": "bar"},
		},
		SandboxConfig: sandbox,
	}
	// Here we pass "" as cniConfig argument of GetVMConfig because we
	// don't test CNI aspect here. It's taken care of in pkg/manager
	// and cloud-init part of this package.
	vmConfig, err := GetVMConfig(req, nil)
	if err != nil {
		ct.t.Fatalf("GetVMConfig(): %v", err)
	}

	containerID, err := ct.virtTool.CreateContainer(vmConfig, "/tmp/fakenetns")
	if err != nil {
		ct.t.Fatalf("CreateContainer: %v", err)
	}
	return containerID
}

func (ct *containerTester) listContainers(filter *kubeapi.ContainerFilter) []*kubeapi.Container {
	containers, err := ct.virtTool.ListContainers(nil)
	if err != nil {
		ct.t.Fatalf("ListContainers() failed: %v", err)
	}
	return containers
}

func (ct *containerTester) containerStatus(containerID string) *kubeapi.ContainerStatus {
	status, err := ct.virtTool.ContainerStatus(containerID)
	if err != nil {
		ct.t.Errorf("ContainerStatus(): %v", err)
	}
	return status
}

func (ct *containerTester) startContainer(containerID string) {
	if err := ct.virtTool.StartContainer(containerID); err != nil {
		ct.t.Fatalf("StartContainer failed for container %q: %v", containerID, err)
	}
}

func (ct *containerTester) stopContainer(containerID string) {
	if err := ct.virtTool.StopContainer(containerID, stopContainerTimeout); err != nil {
		ct.t.Fatalf("StopContainer failed for container %q: %v", containerID, err)
	}
}

func (ct *containerTester) removeContainer(containerID string) {
	if err := ct.virtTool.RemoveContainer(containerID); err != nil {
		ct.t.Fatalf("RemoveContainer failed for container %q: %v", containerID, err)
	}
}

func (ct *containerTester) verifyContainerRootfsExists(container *kubeapi.Container) bool {
	storagePool, err := ct.storageConn.LookupStoragePoolByName("volumes")
	if err != nil {
		ct.t.Fatalf("Expected to found 'volumes' storage pool but failed with: %v", err)
	}
	// TODO: this is third place where rootfs volume name is calculated
	// so there should be a func which will do it in consistent way there,
	// in virtlet_root_volumesource.go and in virtualization.go
	_, err = storagePool.LookupVolumeByName("virtlet_root_" + container.PodSandboxId)
	return err == nil
}

func TestContainerLifecycle(t *testing.T) {
	ct := newContainerTester(t, testutils.NewToplevelRecorder())
	defer ct.teardown()

	sandbox := criapi.GetSandboxes(1)[0]
	ct.setPodSandbox(sandbox)

	containers := ct.listContainers(nil)
	if len(containers) != 0 {
		t.Errorf("Unexpected containers when no containers are started: %#v", containers)
	}

	containerID := ct.createContainer(sandbox, nil)

	containers = ct.listContainers(nil)
	if len(containers) != 1 {
		t.Errorf("Expected single container to be started, but got: %#v", containers)
	}
	container := containers[0]
	if container.Id != containerID {
		t.Errorf("Bad container id in response: %q instead of %q", containers[0].Id, containerID)
	}
	if container.State != kubeapi.ContainerState_CONTAINER_CREATED {
		t.Errorf("Bad container state: %v instead of %v", containers[0].State, kubeapi.ContainerState_CONTAINER_CREATED)
	}
	if container.Metadata.Name != fakeContainerName {
		t.Errorf("Bad container name: %q instead of %q", containers[0].Metadata.Name, fakeContainerName)
	}
	if container.Metadata.Attempt != fakeContainerAttempt {
		t.Errorf("Bad container attempt: %d instead of %d", containers[0].Metadata.Attempt, fakeContainerAttempt)
	}
	if container.Labels[kubetypes.KubernetesContainerNameLabel] != fakeContainerName {
		t.Errorf("Bad container name label: %q instead of %q", containers[0].Labels[kubetypes.KubernetesContainerNameLabel], fakeContainerName)
	}
	if container.Annotations["foo"] != "bar" {
		t.Errorf("Bad container annotation value: %q instead of %q", containers[0].Annotations["foo"], "bar")
	}
	ct.rec.Rec("container list after the container is created", containers)

	ct.clock.Advance(1 * time.Second)
	ct.startContainer(containerID)

	status := ct.containerStatus(containerID)
	if status.State != kubeapi.ContainerState_CONTAINER_RUNNING {
		t.Errorf("Bad container state: %v instead of %v", status.State, kubeapi.ContainerState_CONTAINER_RUNNING)
	}
	ct.rec.Rec("container status after the container is started", status)

	ct.stopContainer(containerID)

	status = ct.containerStatus(containerID)
	if status.State != kubeapi.ContainerState_CONTAINER_EXITED {
		t.Errorf("Bad container state: %v instead of %v", status.State, kubeapi.ContainerState_CONTAINER_EXITED)
	}
	if status.Metadata.Name != fakeContainerName {
		t.Errorf("Bad container name: %q instead of %q", status.Metadata.Name, fakeContainerName)
	}
	if status.Metadata.Attempt != fakeContainerAttempt {
		t.Errorf("Bad container attempt: %d instead of %d", status.Metadata.Attempt, fakeContainerAttempt)
	}
	if status.Labels[kubetypes.KubernetesContainerNameLabel] != fakeContainerName {
		t.Errorf("Bad container name label: %q instead of %q", containers[0].Labels[kubetypes.KubernetesContainerNameLabel], fakeContainerName)
	}
	if status.Annotations["foo"] != "bar" {
		t.Errorf("Bad container annotation value: %q instead of %q", status.Annotations["foo"], "bar")
	}
	ct.rec.Rec("container status after the container is stopped", status)

	ct.removeContainer(containerID)

	containers = ct.listContainers(nil)
	if len(containers) != 0 {
		t.Errorf("Unexpected containers when no containers are started: %#v", containers)
	}

	if ct.verifyContainerRootfsExists(container) {
		t.Errorf("Rootfs volume was not deleted for the container: %#v", container)
	}

	gm.Verify(t, gm.NewYamlVerifier(ct.rec.Content()))
}

func TestDomainForcedShutdown(t *testing.T) {
	ct := newContainerTester(t, testutils.NewToplevelRecorder())
	defer ct.teardown()

	sandbox := criapi.GetSandboxes(1)[0]
	ct.setPodSandbox(sandbox)

	containerID := ct.createContainer(sandbox, nil)
	ct.clock.Advance(1 * time.Second)
	ct.startContainer(containerID)

	ct.domainConn.SetIgnoreShutdown(true)
	go func() {
		// record a couple of ignored shutdown attempts before container destruction
		ct.clock.BlockUntil(1)
		ct.clock.Advance(6 * time.Second)
		ct.clock.BlockUntil(1)
		ct.clock.Advance(6 * time.Second)
		ct.clock.BlockUntil(1)
		ct.clock.Advance(30 * time.Second)
	}()

	ct.rec.Rec("invoking StopContainer()", nil)
	ct.stopContainer(containerID)
	status := ct.containerStatus(containerID)
	if status.State != kubeapi.ContainerState_CONTAINER_EXITED {
		t.Errorf("Bad container state: %v instead of %v", status.State, kubeapi.ContainerState_CONTAINER_EXITED)
	}
	ct.rec.Rec("container status after the container is stopped", status)

	ct.rec.Rec("invoking RemoveContainer()", nil)
	ct.removeContainer(containerID)
	gm.Verify(t, gm.NewYamlVerifier(ct.rec.Content()))
}

func TestDoubleStartError(t *testing.T) {
	ct := newContainerTester(t, testutils.NewToplevelRecorder())
	defer ct.teardown()

	sandbox := criapi.GetSandboxes(1)[0]
	ct.setPodSandbox(sandbox)

	containerID := ct.createContainer(sandbox, nil)
	ct.clock.Advance(1 * time.Second)
	ct.startContainer(containerID)
	if err := ct.virtTool.StartContainer(containerID); err == nil {
		t.Errorf("2nd StartContainer() didn't produce an error")
	}
}

type volMount struct {
	name          string
	containerPath string
}

func TestDomainDefinitions(t *testing.T) {
	flexVolumeDriver := flexvolume.NewFlexVolumeDriver(func() string {
		// note that this is only good for just one flexvolume
		return fakeUUID
	}, flexvolume.NullMounter)
	for _, tc := range []struct {
		name        string
		annotations map[string]string
		flexVolumes map[string]map[string]interface{}
		mounts      []volMount
	}{
		{
			name: "plain domain",
		},
		{
			name: "raw devices",
			flexVolumes: map[string]map[string]interface{}{
				"raw": {
					"type": "raw",
					// FIXME: here we depend upon the fact that /dev/loop0
					// indeed exists in the build container. But we shouldn't.
					"path": "/dev/loop0",
				},
			},
		},
		{
			name: "volumes",
			flexVolumes: map[string]map[string]interface{}{
				"vol1": {
					"type": "qcow2",
				},
				"vol2": {
					"type":     "qcow2",
					"capacity": "2MB",
				},
				"vol3": {
					"type": "qcow2",
				},
			},
		},
		{
			name: "vcpu count",
			annotations: map[string]string{
				"VirtletVCPUCount": "4",
			},
		},
		{
			name: "ceph flexvolume",
			flexVolumes: map[string]map[string]interface{}{
				"ceph": {
					"type":    "ceph",
					"monitor": "127.0.0.1:6789",
					"pool":    "libvirt-pool",
					"volume":  "rbd-test-image",
					"secret":  "Zm9vYmFyCg==",
					"user":    "libvirt",
				},
			},
			mounts: []volMount{
				{
					name:          "ceph",
					containerPath: "/var/lib/whatever",
				},
			},
		},
		{
			name: "cloud-init",
			annotations: map[string]string{
				"VirtletSSHKeys": "key1\nkey2",
			},
		},
		{
			name: "cloud-init with user data",
			annotations: map[string]string{
				"VirtletSSHKeys": "key1\nkey2",
				"VirtletCloudInitUserData": `
                                  users:
                                  - name: cloudy`,
			},
		},
		{
			name: "virtio disk driver",
			annotations: map[string]string{
				"VirtletDiskDriver": "virtio",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := testutils.NewToplevelRecorder()

			ct := newContainerTester(t, rec)
			defer ct.teardown()

			sandbox := criapi.GetSandboxes(1)[0]
			sandbox.Annotations = tc.annotations
			ct.setPodSandbox(sandbox)

			for name, def := range tc.flexVolumes {
				targetDir := filepath.Join(ct.kubeletRootDir, sandbox.Metadata.Uid, "volumes/virtlet~flexvolume_driver", name)
				resultStr := flexVolumeDriver.Run([]string{"mount", targetDir, utils.MapToJSON(def)})
				var r map[string]interface{}
				if err := json.Unmarshal([]byte(resultStr), &r); err != nil {
					t.Errorf("failed to unmarshal flexvolume definition")
					continue
				}
				if r["status"] != "Success" {
					t.Errorf("mounting flexvolume %q failed: %s", name, r["message"])
				}
			}

			var mounts []*kubeapi.Mount
			for _, m := range tc.mounts {
				mounts = append(mounts, &kubeapi.Mount{
					HostPath:      filepath.Join(ct.kubeletRootDir, sandbox.Metadata.Uid, "volumes/virtlet~flexvolume_driver", m.name),
					ContainerPath: m.containerPath,
				})
			}
			containerID := ct.createContainer(sandbox, mounts)
			// startContainer will cause fake Domain
			// to dump the cloudinit iso content
			ct.startContainer(containerID)
			ct.removeContainer(containerID)
			gm.Verify(t, gm.NewYamlVerifier(ct.rec.Content()))
		})
	}
}

func TestDomainResourceConstraints(t *testing.T) {
	cpuQuota := 25000
	cpuPeriod := 100000
	cpuShares := 100
	memoryLimit := 1234567
	cpuCount := 2

	rec := testutils.NewToplevelRecorder()
	rec.AddFilter("DefineDomain")
	ct := newContainerTester(t, rec)
	defer ct.teardown()
	sandbox := criapi.GetSandboxes(1)[0]
	sandbox.Annotations = map[string]string{
		"VirtletVCPUCount": strconv.Itoa(cpuCount),
	}
	ct.setPodSandbox(sandbox)
	req := &kubeapi.CreateContainerRequest{
		PodSandboxId: sandbox.Metadata.Uid,
		Config: &kubeapi.ContainerConfig{
			Metadata: &kubeapi.ContainerMetadata{
				Name:    fakeContainerName,
				Attempt: fakeContainerAttempt,
			},
			Image: &kubeapi.ImageSpec{
				Image: fakeImageName,
			},
			Linux: &kubeapi.LinuxContainerConfig{
				Resources: &kubeapi.LinuxContainerResources{
					CpuQuota:           int64(cpuQuota),
					CpuPeriod:          int64(cpuPeriod),
					CpuShares:          int64(cpuShares),
					MemoryLimitInBytes: int64(memoryLimit),
				},
			},
		},
		SandboxConfig: sandbox,
	}

	vmConfig, err := GetVMConfig(req, nil)
	if err != nil {
		t.Fatalf("GetVMConfig(): %v", err)
	}

	if _, err = ct.virtTool.CreateContainer(vmConfig, "/tmp/fakenetns"); err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}

	gm.Verify(t, gm.NewYamlVerifier(ct.rec.Content()))
}
