// +build linux

/*
Copyright 2015 The Kubernetes Authors.

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

package cadvisor

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"path"
	"time"

	// Register supported container handlers.
	// 在原始的cdvisor是在https://github.com/google/cadvisor/cadvisor.go里，
	// import "github.com/google/cadvisor/container/install"包来注册所有handler。
	// "github.com/google/cadvisor/container/install"包会比这里多import "github.com/google/cadvisor/container/mesos/install"
	_ "github.com/google/cadvisor/container/containerd/install"
	_ "github.com/google/cadvisor/container/crio/install"
	_ "github.com/google/cadvisor/container/docker/install"
	_ "github.com/google/cadvisor/container/systemd/install"

	// Register cloud info providers.
	// TODO(#76660): Remove this once the cAdvisor endpoints are removed.
	_ "github.com/google/cadvisor/utils/cloudinfo/aws"
	_ "github.com/google/cadvisor/utils/cloudinfo/azure"
	_ "github.com/google/cadvisor/utils/cloudinfo/gce"

	"github.com/google/cadvisor/cache/memory"
	cadvisormetrics "github.com/google/cadvisor/container"
	"github.com/google/cadvisor/events"
	cadvisorapi "github.com/google/cadvisor/info/v1"
	cadvisorapiv2 "github.com/google/cadvisor/info/v2"
	"github.com/google/cadvisor/manager"
	"github.com/google/cadvisor/utils/sysfs"
	"k8s.io/klog"
)

type cadvisorClient struct {
	imageFsInfoProvider ImageFsInfoProvider
	rootPath            string
	manager.Manager
}

var _ Interface = new(cadvisorClient)

// TODO(vmarmol): Make configurable.
// The amount of time for which to keep stats in memory.
const statsCacheDuration = 2 * time.Minute
const maxHousekeepingInterval = 15 * time.Second
const defaultHousekeepingInterval = 10 * time.Second
const allowDynamicHousekeeping = true

func init() {
	// Override cAdvisor flag defaults.
	flagOverrides := map[string]string{
		// Override the default cAdvisor housekeeping interval.
		"housekeeping_interval": defaultHousekeepingInterval.String(),
		// Disable event storage by default.
		"event_storage_event_limit": "default=0",
		"event_storage_age_limit":   "default=0",
	}
	for name, defaultValue := range flagOverrides {
		if f := flag.Lookup(name); f != nil {
			f.DefValue = defaultValue
			f.Value.Set(defaultValue)
		} else {
			klog.Errorf("Expected cAdvisor flag %q not found", name)
		}
	}
}

// New creates a new cAdvisor Interface for linux systems.
func New(imageFsInfoProvider ImageFsInfoProvider, rootPath string, cgroupRoots []string, usingLegacyStats bool) (Interface, error) {
	sysFs := sysfs.NewRealSysFs()

	includedMetrics := cadvisormetrics.MetricSet{
		cadvisormetrics.CpuUsageMetrics:         struct{}{},
		cadvisormetrics.MemoryUsageMetrics:      struct{}{},
		cadvisormetrics.CpuLoadMetrics:          struct{}{},
		cadvisormetrics.DiskIOMetrics:           struct{}{},
		cadvisormetrics.NetworkUsageMetrics:     struct{}{},
		cadvisormetrics.AcceleratorUsageMetrics: struct{}{},
		cadvisormetrics.AppMetrics:              struct{}{},
		cadvisormetrics.ProcessMetrics:          struct{}{},
	}
	if usingLegacyStats {
		includedMetrics[cadvisormetrics.DiskUsageMetrics] = struct{}{}
	}

	// Create the cAdvisor container manager.
	m, err := manager.New(memory.New(statsCacheDuration, nil), sysFs, maxHousekeepingInterval, allowDynamicHousekeeping, includedMetrics, http.DefaultClient, cgroupRoots)
	if err != nil {
		return nil, err
	}

	if _, err := os.Stat(rootPath); err != nil {
		if os.IsNotExist(err) {
			if err := os.MkdirAll(path.Clean(rootPath), 0750); err != nil {
				return nil, fmt.Errorf("error creating root directory %q: %v", rootPath, err)
			}
		} else {
			return nil, fmt.Errorf("failed to Stat %q: %v", rootPath, err)
		}
	}

	return &cadvisorClient{
		imageFsInfoProvider: imageFsInfoProvider,
		rootPath:            rootPath,
		Manager:             m,
	}, nil
}

func (cc *cadvisorClient) Start() error {
	return cc.Manager.Start()
}

func (cc *cadvisorClient) ContainerInfo(name string, req *cadvisorapi.ContainerInfoRequest) (*cadvisorapi.ContainerInfo, error) {
	return cc.GetContainerInfo(name, req)
}

// 获得容器的v2.ContainerInfo包括ContainerSpec（包括各种（是否有cpu、内存、网络、blkio、pid等）属性）和ContainerStats（容器的监控状态）
// 如果options.Recursive为true,则包含所有子容器的ContainerInfo
func (cc *cadvisorClient) ContainerInfoV2(name string, options cadvisorapiv2.RequestOptions) (map[string]cadvisorapiv2.ContainerInfo, error) {
	return cc.GetContainerInfoV2(name, options)
}

func (cc *cadvisorClient) VersionInfo() (*cadvisorapi.VersionInfo, error) {
	return cc.GetVersionInfo()
}

func (cc *cadvisorClient) SubcontainerInfo(name string, req *cadvisorapi.ContainerInfoRequest) (map[string]*cadvisorapi.ContainerInfo, error) {
	infos, err := cc.SubcontainersInfo(name, req)
	if err != nil && len(infos) == 0 {
		return nil, err
	}

	result := make(map[string]*cadvisorapi.ContainerInfo, len(infos))
	for _, info := range infos {
		result[info.Name] = info
	}
	return result, err
}

func (cc *cadvisorClient) MachineInfo() (*cadvisorapi.MachineInfo, error) {
	return cc.GetMachineInfo()
}

// 获得镜像保存的磁盘设备最近状态（磁盘设备名、状态的时间、磁盘大小、可用大小、使用量、label列表、inode使用情况），磁盘可能有多个
func (cc *cadvisorClient) ImagesFsInfo() (cadvisorapiv2.FsInfo, error) {
	// 如果runtime是docker，则返回"docker-images"
	// 如果runtime是remote，如果runtimeEndpoint为"/var/run/crio/crio.sock"或"unix:///var/run/crio/crio.sock"（即运行时为cri-o），返回"crio-images"
	// 其他runtime，不支持，返回错误
	label, err := cc.imageFsInfoProvider.ImageFsInfoLabel()
	if err != nil {
		return cadvisorapiv2.FsInfo{}, err
	}
	// 获得这个label的磁盘设备最近状态（磁盘设备名、状态的时间、磁盘大小、可用大小、使用量、label列表），磁盘可能有多个
	return cc.getFsInfo(label)
}

// 返回kubelet root目录所在挂载设备的最近状态（磁盘设备名、状态的时间、磁盘大小、可用大小、使用量、label列表、inode使用情况）
func (cc *cadvisorClient) RootFsInfo() (cadvisorapiv2.FsInfo, error) {
	// 返回目录所在挂载设备的最近状态（磁盘设备名、状态的时间、磁盘大小、可用大小、使用量、label列表、inode使用情况）
	return cc.GetDirFsInfo(cc.rootPath)
}

// 获得这个label的磁盘设备最近状态（磁盘设备名、状态的时间、磁盘大小、可用大小、使用量、label列表），磁盘可能有多个
func (cc *cadvisorClient) getFsInfo(label string) (cadvisorapiv2.FsInfo, error) {
	// 获得这个label的磁盘设备最近状态（磁盘设备名、状态的时间、磁盘大小、可用大小、使用量、label列表），磁盘可能有多个
	res, err := cc.GetFsInfo(label)
	if err != nil {
		return cadvisorapiv2.FsInfo{}, err
	}
	if len(res) == 0 {
		return cadvisorapiv2.FsInfo{}, fmt.Errorf("failed to find information for the filesystem labeled %q", label)
	}
	// TODO(vmarmol): Handle this better when a label has more than one image filesystem.
	if len(res) > 1 {
		klog.Warningf("More than one filesystem labeled %q: %#v. Only using the first one", label, res)
	}

	return res[0], nil
}

func (cc *cadvisorClient) WatchEvents(request *events.Request) (*events.EventChannel, error) {
	return cc.WatchForEvents(request)
}
