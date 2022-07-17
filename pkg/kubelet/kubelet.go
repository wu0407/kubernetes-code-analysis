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

package kubelet

import (
	"context"
	"crypto/tls"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	cadvisorapi "github.com/google/cadvisor/info/v1"
	utilexec "k8s.io/utils/exec"
	"k8s.io/utils/integer"
	"k8s.io/utils/mount"
	utilnet "k8s.io/utils/net"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/clock"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	clientset "k8s.io/client-go/kubernetes"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/certificate"
	"k8s.io/client-go/util/flowcontrol"
	cloudprovider "k8s.io/cloud-provider"
	internalapi "k8s.io/cri-api/pkg/apis"
	"k8s.io/klog"
	pluginwatcherapi "k8s.io/kubelet/pkg/apis/pluginregistration/v1"
	api "k8s.io/kubernetes/pkg/apis/core"
	"k8s.io/kubernetes/pkg/features"
	kubeletconfiginternal "k8s.io/kubernetes/pkg/kubelet/apis/config"
	"k8s.io/kubernetes/pkg/kubelet/apis/podresources"
	"k8s.io/kubernetes/pkg/kubelet/cadvisor"
	kubeletcertificate "k8s.io/kubernetes/pkg/kubelet/certificate"
	"k8s.io/kubernetes/pkg/kubelet/checkpointmanager"
	"k8s.io/kubernetes/pkg/kubelet/cloudresource"
	"k8s.io/kubernetes/pkg/kubelet/cm"
	"k8s.io/kubernetes/pkg/kubelet/config"
	"k8s.io/kubernetes/pkg/kubelet/configmap"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/dockershim"
	dockerremote "k8s.io/kubernetes/pkg/kubelet/dockershim/remote"
	"k8s.io/kubernetes/pkg/kubelet/events"
	"k8s.io/kubernetes/pkg/kubelet/eviction"
	"k8s.io/kubernetes/pkg/kubelet/images"
	"k8s.io/kubernetes/pkg/kubelet/kubeletconfig"
	"k8s.io/kubernetes/pkg/kubelet/kuberuntime"
	"k8s.io/kubernetes/pkg/kubelet/lifecycle"
	"k8s.io/kubernetes/pkg/kubelet/logs"
	"k8s.io/kubernetes/pkg/kubelet/metrics"
	"k8s.io/kubernetes/pkg/kubelet/metrics/collectors"
	"k8s.io/kubernetes/pkg/kubelet/network/dns"
	"k8s.io/kubernetes/pkg/kubelet/nodelease"
	oomwatcher "k8s.io/kubernetes/pkg/kubelet/oom"
	"k8s.io/kubernetes/pkg/kubelet/pleg"
	"k8s.io/kubernetes/pkg/kubelet/pluginmanager"
	plugincache "k8s.io/kubernetes/pkg/kubelet/pluginmanager/cache"
	kubepod "k8s.io/kubernetes/pkg/kubelet/pod"
	"k8s.io/kubernetes/pkg/kubelet/preemption"
	"k8s.io/kubernetes/pkg/kubelet/prober"
	proberesults "k8s.io/kubernetes/pkg/kubelet/prober/results"
	"k8s.io/kubernetes/pkg/kubelet/remote"
	"k8s.io/kubernetes/pkg/kubelet/runtimeclass"
	"k8s.io/kubernetes/pkg/kubelet/secret"
	"k8s.io/kubernetes/pkg/kubelet/server"
	servermetrics "k8s.io/kubernetes/pkg/kubelet/server/metrics"
	serverstats "k8s.io/kubernetes/pkg/kubelet/server/stats"
	"k8s.io/kubernetes/pkg/kubelet/server/streaming"
	"k8s.io/kubernetes/pkg/kubelet/stats"
	"k8s.io/kubernetes/pkg/kubelet/status"
	"k8s.io/kubernetes/pkg/kubelet/sysctl"
	"k8s.io/kubernetes/pkg/kubelet/token"
	kubetypes "k8s.io/kubernetes/pkg/kubelet/types"
	"k8s.io/kubernetes/pkg/kubelet/util"
	"k8s.io/kubernetes/pkg/kubelet/util/format"
	"k8s.io/kubernetes/pkg/kubelet/util/manager"
	"k8s.io/kubernetes/pkg/kubelet/util/queue"
	"k8s.io/kubernetes/pkg/kubelet/util/sliceutils"
	"k8s.io/kubernetes/pkg/kubelet/volumemanager"
	"k8s.io/kubernetes/pkg/security/apparmor"
	sysctlwhitelist "k8s.io/kubernetes/pkg/security/podsecuritypolicy/sysctl"
	utilipt "k8s.io/kubernetes/pkg/util/iptables"
	nodeutil "k8s.io/kubernetes/pkg/util/node"
	"k8s.io/kubernetes/pkg/util/oom"
	"k8s.io/kubernetes/pkg/util/selinux"
	"k8s.io/kubernetes/pkg/volume"
	"k8s.io/kubernetes/pkg/volume/csi"
	"k8s.io/kubernetes/pkg/volume/util/hostutil"
	"k8s.io/kubernetes/pkg/volume/util/subpath"
	"k8s.io/kubernetes/pkg/volume/util/volumepathhandler"
)

const (
	// Max amount of time to wait for the container runtime to come up.
	maxWaitForContainerRuntime = 30 * time.Second

	// nodeStatusUpdateRetry specifies how many times kubelet retries when posting node status failed.
	nodeStatusUpdateRetry = 5

	// ContainerLogsDir is the location of container logs.
	ContainerLogsDir = "/var/log/containers"

	// MaxContainerBackOff is the max backoff period, exported for the e2e test
	MaxContainerBackOff = 300 * time.Second

	// Capacity of the channel for storing pods to kill. A small number should
	// suffice because a goroutine is dedicated to check the channel and does
	// not block on anything else.
	podKillingChannelCapacity = 50

	// Period for performing global cleanup tasks.
	housekeepingPeriod = time.Second * 2

	// Period for performing eviction monitoring.
	// TODO ensure this is in sync with internal cadvisor housekeeping.
	evictionMonitoringPeriod = time.Second * 10

	// The path in containers' filesystems where the hosts file is mounted.
	etcHostsPath = "/etc/hosts"

	// Capacity of the channel for receiving pod lifecycle events. This number
	// is a bit arbitrary and may be adjusted in the future.
	plegChannelCapacity = 1000

	// Generic PLEG relies on relisting for discovering container events.
	// A longer period means that kubelet will take longer to detect container
	// changes and to update pod status. On the other hand, a shorter period
	// will cause more frequent relisting (e.g., container runtime operations),
	// leading to higher cpu usage.
	// Note that even though we set the period to 1s, the relisting itself can
	// take more than 1s to finish if the container runtime responds slowly
	// and/or when there are many container changes in one cycle.
	plegRelistPeriod = time.Second * 1

	// backOffPeriod is the period to back off when pod syncing results in an
	// error. It is also used as the base period for the exponential backoff
	// container restarts and image pulls.
	backOffPeriod = time.Second * 10

	// ContainerGCPeriod is the period for performing container garbage collection.
	ContainerGCPeriod = time.Minute
	// ImageGCPeriod is the period for performing image garbage collection.
	ImageGCPeriod = 5 * time.Minute

	// Minimum number of dead containers to keep in a pod
	minDeadContainerInPod = 1
)

// SyncHandler is an interface implemented by Kubelet, for testability
type SyncHandler interface {
	HandlePodAdditions(pods []*v1.Pod)
	HandlePodUpdates(pods []*v1.Pod)
	HandlePodRemoves(pods []*v1.Pod)
	HandlePodReconcile(pods []*v1.Pod)
	HandlePodSyncs(pods []*v1.Pod)
	HandlePodCleanups() error
}

// Option is a functional option type for Kubelet
type Option func(*Kubelet)

// Bootstrap is a bootstrapping interface for kubelet, targets the initialization protocol
type Bootstrap interface {
	GetConfiguration() kubeletconfiginternal.KubeletConfiguration
	BirthCry()
	StartGarbageCollection()
	ListenAndServe(address net.IP, port uint, tlsOptions *server.TLSOptions, auth server.AuthInterface, enableCAdvisorJSONEndpoints, enableDebuggingHandlers, enableContentionProfiling bool)
	ListenAndServeReadOnly(address net.IP, port uint, enableCAdvisorJSONEndpoints bool)
	ListenAndServePodResources()
	Run(<-chan kubetypes.PodUpdate)
	RunOnce(<-chan kubetypes.PodUpdate) ([]RunPodResult, error)
}

// Builder creates and initializes a Kubelet instance
type Builder func(kubeCfg *kubeletconfiginternal.KubeletConfiguration,
	kubeDeps *Dependencies,
	crOptions *config.ContainerRuntimeOptions,
	containerRuntime string,
	runtimeCgroups string,
	hostnameOverride string,
	nodeIP string,
	providerID string,
	cloudProvider string,
	certDirectory string,
	rootDirectory string,
	registerNode bool,
	registerWithTaints []api.Taint,
	allowedUnsafeSysctls []string,
	remoteRuntimeEndpoint string,
	remoteImageEndpoint string,
	experimentalMounterPath string,
	experimentalKernelMemcgNotification bool,
	experimentalCheckNodeCapabilitiesBeforeMount bool,
	experimentalNodeAllocatableIgnoreEvictionThreshold bool,
	minimumGCAge metav1.Duration,
	maxPerPodContainerCount int32,
	maxContainerCount int32,
	masterServiceNamespace string,
	registerSchedulable bool,
	nonMasqueradeCIDR string,
	keepTerminatedPodVolumes bool,
	nodeLabels map[string]string,
	seccompProfileRoot string,
	bootstrapCheckpointPath string,
	nodeStatusMaxImages int32) (Bootstrap, error)

// Dependencies is a bin for things we might consider "injected dependencies" -- objects constructed
// at runtime that are necessary for running the Kubelet. This is a temporary solution for grouping
// these objects while we figure out a more comprehensive dependency injection story for the Kubelet.
type Dependencies struct {
	Options []Option

	// Injected Dependencies
	Auth                    server.AuthInterface
	CAdvisorInterface       cadvisor.Interface
	Cloud                   cloudprovider.Interface
	ContainerManager        cm.ContainerManager
	DockerClientConfig      *dockershim.ClientConfig
	EventClient             v1core.EventsGetter
	HeartbeatClient         clientset.Interface
	OnHeartbeatFailure      func()
	KubeClient              clientset.Interface
	Mounter                 mount.Interface
	HostUtil                hostutil.HostUtils
	OOMAdjuster             *oom.OOMAdjuster
	OSInterface             kubecontainer.OSInterface
	PodConfig               *config.PodConfig
	Recorder                record.EventRecorder
	Subpather               subpath.Interface
	VolumePlugins           []volume.VolumePlugin
	DynamicPluginProber     volume.DynamicPluginProber
	TLSOptions              *server.TLSOptions
	KubeletConfigController *kubeletconfig.Controller
	RemoteRuntimeService    internalapi.RuntimeService
	RemoteImageService      internalapi.ImageManagerService
	criHandler              http.Handler
	dockerLegacyService     dockershim.DockerLegacyService
	// remove it after cadvisor.UsingLegacyCadvisorStats dropped.
	useLegacyCadvisorStats bool
}

// makePodSourceConfig creates a config.PodConfig from the given
// KubeletConfiguration or returns an error.
func makePodSourceConfig(kubeCfg *kubeletconfiginternal.KubeletConfiguration, kubeDeps *Dependencies, nodeName types.NodeName, bootstrapCheckpointPath string) (*config.PodConfig, error) {
	manifestURLHeader := make(http.Header)
	if len(kubeCfg.StaticPodURLHeader) > 0 {
		for k, v := range kubeCfg.StaticPodURLHeader {
			for i := range v {
				manifestURLHeader.Add(k, v[i])
			}
		}
	}

	// source of all configuration
	cfg := config.NewPodConfig(config.PodConfigNotificationIncremental, kubeDeps.Recorder)

	// define file config source
	// 默认StaticPodPath为空，FileCheckFrequency为20s
	if kubeCfg.StaticPodPath != "" {
		klog.Infof("Adding pod path: %v", kubeCfg.StaticPodPath)
		// 启动goroutine watch文件变化和周期性读取文件，生成PodUpdate（op为set，包含所有pod）消息发送给updates通道
		config.NewSourceFile(kubeCfg.StaticPodPath, nodeName, kubeCfg.FileCheckFrequency.Duration, cfg.Channel(kubetypes.FileSource))
	}

	// define url config source
	// 默认StaticPodURL为空，HTTPCheckFrequency默认为20s
	if kubeCfg.StaticPodURL != "" {
		klog.Infof("Adding pod url %q with HTTP header %v", kubeCfg.StaticPodURL, manifestURLHeader)
		// 启动goroutine 周期请求url获取pod或podlist，生成PodUpdate（op为set，包含所有pod）消息发送给updates通道
		config.NewSourceURL(kubeCfg.StaticPodURL, manifestURLHeader, nodeName, kubeCfg.HTTPCheckFrequency.Duration, cfg.Channel(kubetypes.HTTPSource))
	}

	// Restore from the checkpoint path
	// NOTE: This MUST happen before creating the apiserver source
	// below, or the checkpoint would override the source of truth.

	var updatechannel chan<- interface{}
	// 默认bootstrapCheckpointPath为空
	if bootstrapCheckpointPath != "" {
		klog.Infof("Adding checkpoint path: %v", bootstrapCheckpointPath)
		updatechannel = cfg.Channel(kubetypes.ApiserverSource)
		// 从目录中读取所有checkpoint文件获取所有pod，然后发送PodUpdate（op为restore）到updatechannel
		err := cfg.Restore(bootstrapCheckpointPath, updatechannel)
		if err != nil {
			return nil, err
		}
	}

	if kubeDeps.KubeClient != nil {
		klog.Infof("Watching apiserver")
		if updatechannel == nil {
			updatechannel = cfg.Channel(kubetypes.ApiserverSource)
		}
		config.NewSourceApiserver(kubeDeps.KubeClient, nodeName, updatechannel)
	}
	return cfg, nil
}

// PreInitRuntimeService will init runtime service before RunKubelet.
func PreInitRuntimeService(kubeCfg *kubeletconfiginternal.KubeletConfiguration,
	kubeDeps *Dependencies,
	crOptions *config.ContainerRuntimeOptions,
	containerRuntime string,
	runtimeCgroups string,
	remoteRuntimeEndpoint string,
	remoteImageEndpoint string,
	nonMasqueradeCIDR string) error {
	if remoteRuntimeEndpoint != "" {
		// remoteImageEndpoint is same as remoteRuntimeEndpoint if not explicitly specified
		// 默认--image-service-endpoint就是空
		if remoteImageEndpoint == "" {
			remoteImageEndpoint = remoteRuntimeEndpoint
		}
	}

	switch containerRuntime {
	case kubetypes.DockerContainerRuntime:
		// TODO: These need to become arguments to a standalone docker shim.
		pluginSettings := dockershim.NetworkPluginSettings{
			// 默认promiscuous-bridge
			HairpinMode:        kubeletconfiginternal.HairpinMode(kubeCfg.HairpinMode),
			// 默认为"10.0.0.0/8"
			NonMasqueradeCIDR:  nonMasqueradeCIDR,
			PluginName:         crOptions.NetworkPluginName,
			// 默认/etc/cni/net.d
			PluginConfDir:      crOptions.CNIConfDir,
			// 默认/opt/cni/bin
			PluginBinDirString: crOptions.CNIBinDir,
			// 默认/var/lib/cni/cache
			PluginCacheDir:     crOptions.CNICacheDir,
			// 默认为0--相当于1460/或者网卡的mtu
			MTU:                int(crOptions.NetworkPluginMTU),
		}

		// Create and start the CRI shim running as a grpc server.
		streamingConfig := getStreamingConfig(kubeCfg, kubeDeps, crOptions)
		ds, err := dockershim.NewDockerService(kubeDeps.DockerClientConfig, crOptions.PodSandboxImage, streamingConfig,
			&pluginSettings, runtimeCgroups, kubeCfg.CgroupDriver, crOptions.DockershimRootDirectory, !crOptions.RedirectContainerStreaming)
		if err != nil {
			return err
		}
		// RedirectContainerStreaming默认为false
		if crOptions.RedirectContainerStreaming {
			kubeDeps.criHandler = ds
		}

		// The unix socket for kubelet <-> dockershim communication, dockershim start before runtime service init.
		klog.V(5).Infof("RemoteRuntimeEndpoint: %q, RemoteImageEndpoint: %q",
			remoteRuntimeEndpoint,
			remoteImageEndpoint)
		klog.V(2).Infof("Starting the GRPC server for the docker CRI shim.")
		dockerServer := dockerremote.NewDockerServer(remoteRuntimeEndpoint, ds)
		// 启动stream server处理exec、log等操作--默认监听随机端口
		// 启动goroutine每5分钟，当设置--runtime-cgoups，设置dockerd在相应的cgroup；然后设置dockerd的oom_score_adj为-999
		// 监听dockershim的grpc的socket:///var/run/dockershim.sock
		if err := dockerServer.Start(); err != nil {
			return err
		}

		// Create dockerLegacyService when the logging driver is not supported.
		// docker日志格式为"json-file"，返回true, nil
		// 非json-file格式的日志创建dockerLegacyService
		supported, err := ds.IsCRISupportedLogDriver()
		if err != nil {
			return err
		}
		if !supported {
			kubeDeps.dockerLegacyService = ds
		}
	case kubetypes.RemoteContainerRuntime:
		// No-op.
		break
	default:
		return fmt.Errorf("unsupported CRI runtime: %q", containerRuntime)
	}

	var err error
	// RuntimeRequestTimeout默认为2分钟
	if kubeDeps.RemoteRuntimeService, err = remote.NewRemoteRuntimeService(remoteRuntimeEndpoint, kubeCfg.RuntimeRequestTimeout.Duration); err != nil {
		return err
	}
	if kubeDeps.RemoteImageService, err = remote.NewRemoteImageService(remoteImageEndpoint, kubeCfg.RuntimeRequestTimeout.Duration); err != nil {
		return err
	}

	//linux上的docker或crio的socket都为true
	kubeDeps.useLegacyCadvisorStats = cadvisor.UsingLegacyCadvisorStats(containerRuntime, remoteRuntimeEndpoint)

	return nil
}

// NewMainKubelet instantiates a new Kubelet object along with all the required internal modules.
// No initialization of Kubelet and its modules should happen here.
func NewMainKubelet(kubeCfg *kubeletconfiginternal.KubeletConfiguration,
	kubeDeps *Dependencies,
	crOptions *config.ContainerRuntimeOptions,
	containerRuntime string,
	hostnameOverride string,
	nodeIP string,
	providerID string,
	cloudProvider string,
	certDirectory string,
	rootDirectory string,
	registerNode bool,
	registerWithTaints []api.Taint,
	allowedUnsafeSysctls []string,
	experimentalMounterPath string,
	experimentalKernelMemcgNotification bool,
	experimentalCheckNodeCapabilitiesBeforeMount bool,
	experimentalNodeAllocatableIgnoreEvictionThreshold bool,
	minimumGCAge metav1.Duration,
	maxPerPodContainerCount int32,
	maxContainerCount int32,
	masterServiceNamespace string,
	registerSchedulable bool,
	keepTerminatedPodVolumes bool,
	nodeLabels map[string]string,
	seccompProfileRoot string,
	bootstrapCheckpointPath string,
	nodeStatusMaxImages int32) (*Kubelet, error) {
	if rootDirectory == "" {
		return nil, fmt.Errorf("invalid root directory %q", rootDirectory)
	}
	// 默认为1分钟
	if kubeCfg.SyncFrequency.Duration <= 0 {
		return nil, fmt.Errorf("invalid sync frequency %d", kubeCfg.SyncFrequency.Duration)
	}

	// 默认为true
	if kubeCfg.MakeIPTablesUtilChains {
		if kubeCfg.IPTablesMasqueradeBit > 31 || kubeCfg.IPTablesMasqueradeBit < 0 {
			return nil, fmt.Errorf("iptables-masquerade-bit is not valid. Must be within [0, 31]")
		}
		if kubeCfg.IPTablesDropBit > 31 || kubeCfg.IPTablesDropBit < 0 {
			return nil, fmt.Errorf("iptables-drop-bit is not valid. Must be within [0, 31]")
		}
		if kubeCfg.IPTablesDropBit == kubeCfg.IPTablesMasqueradeBit {
			return nil, fmt.Errorf("iptables-masquerade-bit and iptables-drop-bit must be different")
		}
	}

	// 有hostnameOverride就使用hostnameOverride，否则使用主机名
	hostname, err := nodeutil.GetHostname(hostnameOverride)
	if err != nil {
		return nil, err
	}
	// Query the cloud provider for our node name, default to hostname
	nodeName := types.NodeName(hostname)
	if kubeDeps.Cloud != nil {
		var err error
		instances, ok := kubeDeps.Cloud.Instances()
		if !ok {
			return nil, fmt.Errorf("failed to get instances from cloud provider")
		}

		// cloud provider为aws，aws会返回主机的dns名字
		nodeName, err = instances.CurrentNodeName(context.TODO(), hostname)
		if err != nil {
			return nil, fmt.Errorf("error fetching current instance name from cloud provider: %v", err)
		}

		klog.V(2).Infof("cloud provider determined current node name to be %s", nodeName)
	}

	if kubeDeps.PodConfig == nil {
		var err error
		// bootstrapCheckpointPath默认为空
		kubeDeps.PodConfig, err = makePodSourceConfig(kubeCfg, kubeDeps, nodeName, bootstrapCheckpointPath)
		if err != nil {
			return nil, err
		}
	}

	containerGCPolicy := kubecontainer.ContainerGCPolicy{
		MinAge:             minimumGCAge.Duration,
		MaxPerPodContainer: int(maxPerPodContainerCount),
		MaxContainers:      int(maxContainerCount),
	}

	daemonEndpoints := &v1.NodeDaemonEndpoints{
		// Port默认为10250
		KubeletEndpoint: v1.DaemonEndpoint{Port: kubeCfg.Port},
	}

	imageGCPolicy := images.ImageGCPolicy{
		// 默认为0秒
		MinAge:               kubeCfg.ImageMinimumGCAge.Duration,
		// 默认为85
		HighThresholdPercent: int(kubeCfg.ImageGCHighThresholdPercent),
		// 默认为80
		LowThresholdPercent:  int(kubeCfg.ImageGCLowThresholdPercent),
	}

	// 默认为pods
	enforceNodeAllocatable := kubeCfg.EnforceNodeAllocatable
	if experimentalNodeAllocatableIgnoreEvictionThreshold {
		// Do not provide kubeCfg.EnforceNodeAllocatable to eviction threshold parsing if we are not enforcing Evictions
		enforceNodeAllocatable = []string{}
	}
	// hardEviction与softEviction的区别是hardEviction的GracePeriod一定是0，而softEviction不为0
	// 这也是softThresholds与hardThresholds和results都用slice的原因
	thresholds, err := eviction.ParseThresholdConfig(enforceNodeAllocatable, kubeCfg.EvictionHard, kubeCfg.EvictionSoft, kubeCfg.EvictionSoftGracePeriod, kubeCfg.EvictionMinimumReclaim)
	if err != nil {
		return nil, err
	}
	evictionConfig := eviction.Config{
		// EvictionPressureTransitionPeriod默认为5分钟
		PressureTransitionPeriod: kubeCfg.EvictionPressureTransitionPeriod.Duration,
		MaxPodGracePeriodSeconds: int64(kubeCfg.EvictionMaxPodGracePeriod),
		Thresholds:               thresholds,
		// experimentalKernelMemcgNotification默认为false
		KernelMemcgNotification:  experimentalKernelMemcgNotification,
		// 如果是cgroupdriver是systemd，PodCgroupRoot是/kubepods.slice
		PodCgroupRoot:            kubeDeps.ContainerManager.GetPodCgroupRoot(),
	}

	serviceIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	if kubeDeps.KubeClient != nil {
		serviceLW := cache.NewListWatchFromClient(kubeDeps.KubeClient.CoreV1().RESTClient(), "services", metav1.NamespaceAll, fields.Everything())
		r := cache.NewReflector(serviceLW, &v1.Service{}, serviceIndexer, 0)
		go r.Run(wait.NeverStop)
	}
	serviceLister := corelisters.NewServiceLister(serviceIndexer)

	nodeIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	if kubeDeps.KubeClient != nil {
		fieldSelector := fields.Set{api.ObjectNameField: string(nodeName)}.AsSelector()
		nodeLW := cache.NewListWatchFromClient(kubeDeps.KubeClient.CoreV1().RESTClient(), "nodes", metav1.NamespaceAll, fieldSelector)
		r := cache.NewReflector(nodeLW, &v1.Node{}, nodeIndexer, 0)
		go r.Run(wait.NeverStop)
	}
	nodeLister := corelisters.NewNodeLister(nodeIndexer)

	// TODO: get the real node object of ourself,
	// and use the real node name and UID.
	// TODO: what is namespace for node?
	nodeRef := &v1.ObjectReference{
		Kind:      "Node",
		Name:      string(nodeName),
		UID:       types.UID(nodeName),
		Namespace: "",
	}

	containerRefManager := kubecontainer.NewRefManager()

	oomWatcher, err := oomwatcher.NewWatcher(kubeDeps.Recorder)
	if err != nil {
		return nil, err
	}

	clusterDNS := make([]net.IP, 0, len(kubeCfg.ClusterDNS))
	for _, ipEntry := range kubeCfg.ClusterDNS {
		ip := net.ParseIP(ipEntry)
		if ip == nil {
			klog.Warningf("Invalid clusterDNS ip '%q'", ipEntry)
		} else {
			clusterDNS = append(clusterDNS, ip)
		}
	}
	httpClient := &http.Client{}
	// nodeIp默认为空
	parsedNodeIP := net.ParseIP(nodeIP)
	protocol := utilipt.ProtocolIpv4
	if utilnet.IsIPv6(parsedNodeIP) {
		klog.V(0).Infof("IPv6 node IP (%s), assume IPv6 operation", nodeIP)
		protocol = utilipt.ProtocolIpv6
	}

	klet := &Kubelet{
		hostname:                                hostname,
		hostnameOverridden:                      len(hostnameOverride) > 0,
		// 优先使用cloudprovider提供的主机名，其次是hostnameOverride，最后是主机名
		nodeName:                                nodeName,
		kubeClient:                              kubeDeps.KubeClient,
		heartbeatClient:                         kubeDeps.HeartbeatClient,
		// kubeDeps.OnHeartbeatFailure为closeAllConns
		onRepeatedHeartbeatFailure:              kubeDeps.OnHeartbeatFailure,
		rootDirectory:                           rootDirectory,
		// 默认为1分钟
		resyncInterval:                          kubeCfg.SyncFrequency.Duration,
		sourcesReady:                            config.NewSourcesReady(kubeDeps.PodConfig.SeenAllSources),
		// 默认为true
		registerNode:                            registerNode,
		registerWithTaints:                      registerWithTaints,
		// 默认为true
		registerSchedulable:                     registerSchedulable,
		// kubeCfg.ResolverConfig默认为"/etc/resolv.conf"
		dnsConfigurer:                           dns.NewConfigurer(kubeDeps.Recorder, nodeRef, parsedNodeIP, clusterDNS, kubeCfg.ClusterDomain, kubeCfg.ResolverConfig),
		serviceLister:                           serviceLister,
		nodeLister:                              nodeLister,
		// 默认为default
		masterServiceNamespace:                  masterServiceNamespace,
		// 默认为4小时
		streamingConnectionIdleTimeout:          kubeCfg.StreamingConnectionIdleTimeout.Duration,
		recorder:                                kubeDeps.Recorder,
		cadvisor:                                kubeDeps.CAdvisorInterface,
		cloud:                                   kubeDeps.Cloud,
		externalCloudProvider:                   cloudprovider.IsExternal(cloudProvider),
		// providerID默认为空
		providerID:                              providerID,
		nodeRef:                                 nodeRef,
		nodeLabels:                              nodeLabels,
		// NodeStatusUpdateFrequency默认为10s
		nodeStatusUpdateFrequency:               kubeCfg.NodeStatusUpdateFrequency.Duration,
		// 默认为5分钟
		nodeStatusReportFrequency:               kubeCfg.NodeStatusReportFrequency.Duration,
		os:                                      kubeDeps.OSInterface,
		oomWatcher:                              oomWatcher,
		// 默认为true
		cgroupsPerQOS:                           kubeCfg.CgroupsPerQOS,
		// 这里--cgroups-per-qos enabled, but --cgroup-root was not specified.  defaulting to /"
		cgroupRoot:                              kubeCfg.CgroupRoot,
		mounter:                                 kubeDeps.Mounter,
		hostutil:                                kubeDeps.HostUtil,
		subpather:                               kubeDeps.Subpather,
		maxPods:                                 int(kubeCfg.MaxPods),
		// 默认为0
		podsPerCore:                             int(kubeCfg.PodsPerCore),
		syncLoopMonitor:                         atomic.Value{},
		daemonEndpoints:                         daemonEndpoints,
		containerManager:                        kubeDeps.ContainerManager,
		// 默认为docker
		containerRuntimeName:                    containerRuntime,
		// 默认为false
		redirectContainerStreaming:              crOptions.RedirectContainerStreaming,
		nodeIP:                                  parsedNodeIP,
		nodeIPValidator:                         validateNodeIP,
		clock:                                   clock.RealClock{},
		// 默认为true
		enableControllerAttachDetach:            kubeCfg.EnableControllerAttachDetach,
		iptClient:                               utilipt.New(utilexec.New(), protocol),
		// 默认为true
		makeIPTablesUtilChains:                  kubeCfg.MakeIPTablesUtilChains,
		// 默认为14
		iptablesMasqueradeBit:                   int(kubeCfg.IPTablesMasqueradeBit),
		// 默认为15
		iptablesDropBit:                         int(kubeCfg.IPTablesDropBit),
		// 默认为false
		experimentalHostUserNamespaceDefaulting: utilfeature.DefaultFeatureGate.Enabled(features.ExperimentalHostUserNamespaceDefaultingGate),
		// 默认为false
		keepTerminatedPodVolumes:                keepTerminatedPodVolumes,
		// 默认为50
		nodeStatusMaxImages:                     nodeStatusMaxImages,
	}

	if klet.cloud != nil {
		klet.cloudResourceSyncManager = cloudresource.NewSyncManager(klet.cloud, nodeName, klet.nodeStatusUpdateFrequency)
	}

	var secretManager secret.Manager
	var configMapManager configmap.Manager
	// 默认为watch
	switch kubeCfg.ConfigMapAndSecretChangeDetectionStrategy {
	case kubeletconfiginternal.WatchChangeDetectionStrategy:
		secretManager = secret.NewWatchingSecretManager(kubeDeps.KubeClient)
		configMapManager = configmap.NewWatchingConfigMapManager(kubeDeps.KubeClient)
	case kubeletconfiginternal.TTLCacheChangeDetectionStrategy:
		secretManager = secret.NewCachingSecretManager(
			kubeDeps.KubeClient, manager.GetObjectTTLFromNodeFunc(klet.GetNode))
		configMapManager = configmap.NewCachingConfigMapManager(
			kubeDeps.KubeClient, manager.GetObjectTTLFromNodeFunc(klet.GetNode))
	case kubeletconfiginternal.GetChangeDetectionStrategy:
		secretManager = secret.NewSimpleSecretManager(kubeDeps.KubeClient)
		configMapManager = configmap.NewSimpleConfigMapManager(kubeDeps.KubeClient)
	default:
		return nil, fmt.Errorf("unknown configmap and secret manager mode: %v", kubeCfg.ConfigMapAndSecretChangeDetectionStrategy)
	}

	klet.secretManager = secretManager
	klet.configMapManager = configMapManager

	if klet.experimentalHostUserNamespaceDefaulting {
		klog.Infof("Experimental host user namespace defaulting is enabled.")
	}

	machineInfo, err := klet.cadvisor.MachineInfo()
	if err != nil {
		return nil, err
	}
	klet.machineInfo = machineInfo

	imageBackOff := flowcontrol.NewBackOff(backOffPeriod, MaxContainerBackOff)

	klet.livenessManager = proberesults.NewManager()
	klet.startupManager = proberesults.NewManager()

	klet.podCache = kubecontainer.NewCache()
	var checkpointManager checkpointmanager.CheckpointManager
	// bootstrapCheckpointPath默认为空
	if bootstrapCheckpointPath != "" {
		checkpointManager, err = checkpointmanager.NewCheckpointManager(bootstrapCheckpointPath)
		if err != nil {
			return nil, fmt.Errorf("failed to initialize checkpoint manager: %+v", err)
		}
	}
	// podManager is also responsible for keeping secretManager and configMapManager contents up-to-date.
	mirrorPodClient := kubepod.NewBasicMirrorClient(klet.kubeClient, string(nodeName), nodeLister)
	klet.podManager = kubepod.NewBasicPodManager(mirrorPodClient, secretManager, configMapManager, checkpointManager)

	klet.statusManager = status.NewManager(klet.kubeClient, klet.podManager, klet)

	// VolumeStatsAggPeriod默认1分钟
	klet.resourceAnalyzer = serverstats.NewResourceAnalyzer(klet, kubeCfg.VolumeStatsAggPeriod.Duration)

	klet.dockerLegacyService = kubeDeps.dockerLegacyService
	klet.criHandler = kubeDeps.criHandler
	klet.runtimeService = kubeDeps.RemoteRuntimeService

	// features.RuntimeClass默认启用
	if utilfeature.DefaultFeatureGate.Enabled(features.RuntimeClass) && kubeDeps.KubeClient != nil {
		klet.runtimeClassManager = runtimeclass.NewManager(kubeDeps.KubeClient)
	}

	runtime, err := kuberuntime.NewKubeGenericRuntimeManager(
		kubecontainer.FilterEventRecorder(kubeDeps.Recorder),
		klet.livenessManager,
		klet.startupManager,
		// 默认为/var/lib/kubelet/seccomp
		seccompProfileRoot,
		containerRefManager,
		machineInfo,
		klet,
		kubeDeps.OSInterface,
		klet,
		httpClient,
		imageBackOff,
		kubeCfg.SerializeImagePulls,
		// 默认为5
		float32(kubeCfg.RegistryPullQPS),
		// 默认为10
		int(kubeCfg.RegistryBurst),
		kubeCfg.CPUCFSQuota,
		kubeCfg.CPUCFSQuotaPeriod,
		kubeDeps.RemoteRuntimeService,
		kubeDeps.RemoteImageService,
		kubeDeps.ContainerManager.InternalContainerLifecycle(),
		kubeDeps.dockerLegacyService,
		klet.runtimeClassManager,
	)
	if err != nil {
		return nil, err
	}
	klet.containerRuntime = runtime
	klet.streamingRuntime = runtime
	klet.runner = runtime

	runtimeCache, err := kubecontainer.NewRuntimeCache(klet.containerRuntime)
	if err != nil {
		return nil, err
	}
	klet.runtimeCache = runtimeCache

	if kubeDeps.useLegacyCadvisorStats {
		klet.StatsProvider = stats.NewCadvisorStatsProvider(
			klet.cadvisor,
			klet.resourceAnalyzer,
			klet.podManager,
			klet.runtimeCache,
			klet.containerRuntime,
			klet.statusManager)
	} else {
		klet.StatsProvider = stats.NewCRIStatsProvider(
			klet.cadvisor,
			klet.resourceAnalyzer,
			klet.podManager,
			klet.runtimeCache,
			kubeDeps.RemoteRuntimeService,
			kubeDeps.RemoteImageService,
			stats.NewLogMetricsService(),
			kubecontainer.RealOS{})
	}

	klet.pleg = pleg.NewGenericPLEG(klet.containerRuntime, plegChannelCapacity, plegRelistPeriod, klet.podCache, clock.RealClock{})
	klet.runtimeState = newRuntimeState(maxWaitForContainerRuntime)
	klet.runtimeState.addHealthCheck("PLEG", klet.pleg.Healthy)
	// 如果命令行配置了--pod-cidr或PodCIDR，则设置网络插件的podcidr，kubeCfg.PodCIDR默认为空
	if _, err := klet.updatePodCIDR(kubeCfg.PodCIDR); err != nil {
		klog.Errorf("Pod CIDR update failed %v", err)
	}

	// setup containerGC
	containerGC, err := kubecontainer.NewContainerGC(klet.containerRuntime, containerGCPolicy, klet.sourcesReady)
	if err != nil {
		return nil, err
	}
	klet.containerGC = containerGC
	klet.containerDeletor = newPodContainerDeletor(klet.containerRuntime, integer.IntMax(containerGCPolicy.MaxPerPodContainer, minDeadContainerInPod))

	// setup imageManager
	imageManager, err := images.NewImageGCManager(klet.containerRuntime, klet.StatsProvider, kubeDeps.Recorder, nodeRef, imageGCPolicy, crOptions.PodSandboxImage)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize image manager: %v", err)
	}
	klet.imageManager = imageManager

	if containerRuntime == kubetypes.RemoteContainerRuntime && utilfeature.DefaultFeatureGate.Enabled(features.CRIContainerLogRotation) {
		// setup containerLogManager for CRI container runtime
		containerLogManager, err := logs.NewContainerLogManager(
			klet.runtimeService,
			kubeCfg.ContainerLogMaxSize,
			int(kubeCfg.ContainerLogMaxFiles),
		)
		if err != nil {
			return nil, fmt.Errorf("failed to initialize container log manager: %v", err)
		}
		klet.containerLogManager = containerLogManager
	} else {
		// 不做任何事情
		klet.containerLogManager = logs.NewStubContainerLogManager()
	}

	if kubeCfg.ServerTLSBootstrap && kubeDeps.TLSOptions != nil && utilfeature.DefaultFeatureGate.Enabled(features.RotateKubeletServerCertificate) {
		klet.serverCertificateManager, err = kubeletcertificate.NewKubeletServerCertificateManager(klet.kubeClient, kubeCfg, klet.nodeName, klet.getLastObservedNodeAddresses, certDirectory)
		if err != nil {
			return nil, fmt.Errorf("failed to initialize certificate manager: %v", err)
		}
		kubeDeps.TLSOptions.Config.GetCertificate = func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			cert := klet.serverCertificateManager.Current()
			if cert == nil {
				return nil, fmt.Errorf("no serving certificate available for the kubelet")
			}
			return cert, nil
		}
	}

	klet.probeManager = prober.NewManager(
		klet.statusManager,
		klet.livenessManager,
		klet.startupManager,
		klet.runner,
		containerRefManager,
		kubeDeps.Recorder)

	tokenManager := token.NewManager(kubeDeps.KubeClient)

	// NewInitializedVolumePluginMgr initializes some storageErrors on the Kubelet runtimeState (in csi_plugin.go init)
	// which affects node ready status. This function must be called before Kubelet is initialized so that the Node
	// ReadyState is accurate with the storage state.
	klet.volumePluginMgr, err =
		NewInitializedVolumePluginMgr(klet, secretManager, configMapManager, tokenManager, kubeDeps.VolumePlugins, kubeDeps.DynamicPluginProber)
	if err != nil {
		return nil, err
	}
	klet.pluginManager = pluginmanager.NewPluginManager(
		// 默认目录为/var/lib/kubelet/plugins_registry
		klet.getPluginsRegistrationDir(), /* sockDir */
		kubeDeps.Recorder,
	)

	// If the experimentalMounterPathFlag is set, we do not want to
	// check node capabilities since the mount path is not the default
	// experimentalMounterPath默认为空
	if len(experimentalMounterPath) != 0 {
		experimentalCheckNodeCapabilitiesBeforeMount = false
		// Replace the nameserver in containerized-mounter's rootfs/etc/resolve.conf with kubelet.ClusterDNS
		// so that service name could be resolved
		// 提取出dnsConfigurer.ResolverConfig(一般为宿主机的/etc/resolve.conf)的文件里"search"后面的search domain，和遍历clusterDNS输出"nameserver ip[0]\nnameserver ip[0]"
		// 写入到{experimentalMounterPath}/rootfs/etc/resolv.conf
		// 输出类似
		// nameserver 192.168.1.1
		// nameserver 192.168.1.2
		// seaarch xxx.com xx.com
		klet.dnsConfigurer.SetupDNSinContainerizedMounter(experimentalMounterPath)
	}

	// setup volumeManager
	klet.volumeManager = volumemanager.NewVolumeManager(
		kubeCfg.EnableControllerAttachDetach,
		nodeName,
		klet.podManager,
		klet.statusManager,
		klet.kubeClient,
		klet.volumePluginMgr,
		klet.containerRuntime,
		kubeDeps.Mounter,
		kubeDeps.HostUtil,
		klet.getPodsDir(),
		kubeDeps.Recorder,
		experimentalCheckNodeCapabilitiesBeforeMount,
		keepTerminatedPodVolumes,
		volumepathhandler.NewBlockVolumePathHandler())

	klet.reasonCache = NewReasonCache()
	klet.workQueue = queue.NewBasicWorkQueue(klet.clock)
	klet.podWorkers = newPodWorkers(klet.syncPod, kubeDeps.Recorder, klet.workQueue, klet.resyncInterval, backOffPeriod, klet.podCache)

	klet.backOff = flowcontrol.NewBackOff(backOffPeriod, MaxContainerBackOff)
	klet.podKillingCh = make(chan *kubecontainer.PodPair, podKillingChannelCapacity)

	etcHostsPathFunc := func(podUID types.UID) string { return getEtcHostsPath(klet.getPodDir(podUID)) }
	// setup eviction manager
	evictionManager, evictionAdmitHandler := eviction.NewManager(klet.resourceAnalyzer, evictionConfig, killPodNow(klet.podWorkers, kubeDeps.Recorder), klet.podManager.GetMirrorPodByPod, klet.imageManager, klet.containerGC, kubeDeps.Recorder, nodeRef, klet.clock, etcHostsPathFunc)

	klet.evictionManager = evictionManager
	klet.admitHandlers.AddPodAdmitHandler(evictionAdmitHandler)

	// 默认启用
	if utilfeature.DefaultFeatureGate.Enabled(features.Sysctls) {
		// Safe, whitelisted sysctls can always be used as unsafe sysctls in the spec.
		// Hence, we concatenate those two lists.
		// allowedUnsafeSysctls默认为空
		safeAndUnsafeSysctls := append(sysctlwhitelist.SafeSysctlWhitelist(), allowedUnsafeSysctls...)
		sysctlsWhitelist, err := sysctl.NewWhitelist(safeAndUnsafeSysctls)
		if err != nil {
			return nil, err
		}
		klet.admitHandlers.AddPodAdmitHandler(sysctlsWhitelist)
	}

	// enable active deadline handler
	activeDeadlineHandler, err := newActiveDeadlineHandler(klet.statusManager, kubeDeps.Recorder, klet.clock)
	if err != nil {
		return nil, err
	}
	klet.AddPodSyncLoopHandler(activeDeadlineHandler)
	klet.AddPodSyncHandler(activeDeadlineHandler)

	// 增加containerManagerImpl.topologyManager或containerManagerImpl.resourceAllocator（包括cm.cpuManager, cm.deviceManager）到admitHandlers
	klet.admitHandlers.AddPodAdmitHandler(klet.containerManager.GetAllocateResourcesPodAdmitHandler())

	criticalPodAdmissionHandler := preemption.NewCriticalPodAdmissionHandler(klet.GetActivePods, killPodNow(klet.podWorkers, kubeDeps.Recorder), kubeDeps.Recorder)
	// klet.containerManager.UpdatePluginResources为cm.deviceManager.UpdatePluginResources
	// klet.admitHandlers里面有evictionAdmitHandler、sysctlsWhitelist、containerManagerImpl.topologyManager或containerManagerImpl.resourceAllocator（包括cm.cpuManager, cm.deviceManager）、lifecycle.NewPredicateAdmitHandler
	klet.admitHandlers.AddPodAdmitHandler(lifecycle.NewPredicateAdmitHandler(klet.getNodeAnyWay, criticalPodAdmissionHandler, klet.containerManager.UpdatePluginResources))
	// apply functional Option's
	// 默认没有Options
	for _, opt := range kubeDeps.Options {
		opt(klet)
	}

	// containerRuntime默认为docker
	klet.appArmorValidator = apparmor.NewValidator(containerRuntime)
	klet.softAdmitHandlers.AddPodAdmitHandler(lifecycle.NewAppArmorAdmitHandler(klet.appArmorValidator))
	klet.softAdmitHandlers.AddPodAdmitHandler(lifecycle.NewNoNewPrivsAdmitHandler(klet.containerRuntime))
	klet.softAdmitHandlers.AddPodAdmitHandler(lifecycle.NewProcMountAdmitHandler(klet.containerRuntime))

	// NodeLeaseDurationSeconds默认为40s
	klet.nodeLeaseController = nodelease.NewController(klet.clock, klet.heartbeatClient, string(klet.nodeName), kubeCfg.NodeLeaseDurationSeconds, klet.onRepeatedHeartbeatFailure)

	// Finally, put the most recent version of the config on the Kubelet, so
	// people can see how it was configured.
	klet.kubeletConfiguration = *kubeCfg

	// Generating the status funcs should be the last thing we do,
	// since this relies on the rest of the Kubelet having been constructed.
	klet.setNodeStatusFuncs = klet.defaultNodeStatusFuncs()

	return klet, nil
}

type serviceLister interface {
	List(labels.Selector) ([]*v1.Service, error)
}

// Kubelet is the main kubelet implementation.
type Kubelet struct {
	kubeletConfiguration kubeletconfiginternal.KubeletConfiguration

	// hostname is the hostname the kubelet detected or was given via flag/config
	hostname string
	// hostnameOverridden indicates the hostname was overridden via flag/config
	hostnameOverridden bool

	nodeName        types.NodeName
	runtimeCache    kubecontainer.RuntimeCache
	kubeClient      clientset.Interface
	heartbeatClient clientset.Interface
	iptClient       utilipt.Interface
	rootDirectory   string

	lastObservedNodeAddressesMux sync.RWMutex
	lastObservedNodeAddresses    []v1.NodeAddress

	// onRepeatedHeartbeatFailure is called when a heartbeat operation fails more than once. optional.
	onRepeatedHeartbeatFailure func()

	// podWorkers handle syncing Pods in response to events.
	podWorkers PodWorkers

	// resyncInterval is the interval between periodic full reconciliations of
	// pods on this node.
	resyncInterval time.Duration

	// sourcesReady records the sources seen by the kubelet, it is thread-safe.
	sourcesReady config.SourcesReady

	// podManager is a facade that abstracts away the various sources of pods
	// this Kubelet services.
	podManager kubepod.Manager

	// Needed to observe and respond to situations that could impact node stability
	evictionManager eviction.Manager

	// Optional, defaults to /logs/ from /var/log
	logServer http.Handler
	// Optional, defaults to simple Docker implementation
	runner kubecontainer.ContainerCommandRunner

	// cAdvisor used for container information.
	cadvisor cadvisor.Interface

	// Set to true to have the node register itself with the apiserver.
	registerNode bool
	// List of taints to add to a node object when the kubelet registers itself.
	registerWithTaints []api.Taint
	// Set to true to have the node register itself as schedulable.
	registerSchedulable bool
	// for internal book keeping; access only from within registerWithApiserver
	registrationCompleted bool

	// dnsConfigurer is used for setting up DNS resolver configuration when launching pods.
	dnsConfigurer *dns.Configurer

	// masterServiceNamespace is the namespace that the master service is exposed in.
	masterServiceNamespace string
	// serviceLister knows how to list services
	serviceLister serviceLister
	// nodeLister knows how to list nodes
	nodeLister corelisters.NodeLister

	// a list of node labels to register
	nodeLabels map[string]string

	// Last timestamp when runtime responded on ping.
	// Mutex is used to protect this value.
	runtimeState *runtimeState

	// Volume plugins.
	volumePluginMgr *volume.VolumePluginMgr

	// Handles container probing.
	probeManager prober.Manager
	// Manages container health check results.
	livenessManager proberesults.Manager
	startupManager  proberesults.Manager

	// How long to keep idle streaming command execution/port forwarding
	// connections open before terminating them
	streamingConnectionIdleTimeout time.Duration

	// The EventRecorder to use
	recorder record.EventRecorder

	// Policy for handling garbage collection of dead containers.
	containerGC kubecontainer.ContainerGC

	// Manager for image garbage collection.
	imageManager images.ImageGCManager

	// Manager for container logs.
	containerLogManager logs.ContainerLogManager

	// Secret manager.
	secretManager secret.Manager

	// ConfigMap manager.
	configMapManager configmap.Manager

	// Cached MachineInfo returned by cadvisor.
	machineInfo *cadvisorapi.MachineInfo

	// Handles certificate rotations.
	serverCertificateManager certificate.Manager

	// Syncs pods statuses with apiserver; also used as a cache of statuses.
	statusManager status.Manager

	// VolumeManager runs a set of asynchronous loops that figure out which
	// volumes need to be attached/mounted/unmounted/detached based on the pods
	// scheduled on this node and makes it so.
	volumeManager volumemanager.VolumeManager

	// Cloud provider interface.
	cloud cloudprovider.Interface
	// Handles requests to cloud provider with timeout
	cloudResourceSyncManager cloudresource.SyncManager

	// Indicates that the node initialization happens in an external cloud controller
	externalCloudProvider bool
	// Reference to this node.
	nodeRef *v1.ObjectReference

	// The name of the container runtime
	containerRuntimeName string

	// redirectContainerStreaming enables container streaming redirect.
	redirectContainerStreaming bool

	// Container runtime.
	containerRuntime kubecontainer.Runtime

	// Streaming runtime handles container streaming.
	streamingRuntime kubecontainer.StreamingRuntime

	// Container runtime service (needed by container runtime Start()).
	// TODO(CD): try to make this available without holding a reference in this
	//           struct. For example, by adding a getter to generic runtime.
	runtimeService internalapi.RuntimeService

	// reasonCache caches the failure reason of the last creation of all containers, which is
	// used for generating ContainerStatus.
	reasonCache *ReasonCache

	// nodeStatusUpdateFrequency specifies how often kubelet computes node status. If node lease
	// feature is not enabled, it is also the frequency that kubelet posts node status to master.
	// In that case, be cautious when changing the constant, it must work with nodeMonitorGracePeriod
	// in nodecontroller. There are several constraints:
	// 1. nodeMonitorGracePeriod must be N times more than nodeStatusUpdateFrequency, where
	//    N means number of retries allowed for kubelet to post node status. It is pointless
	//    to make nodeMonitorGracePeriod be less than nodeStatusUpdateFrequency, since there
	//    will only be fresh values from Kubelet at an interval of nodeStatusUpdateFrequency.
	//    The constant must be less than podEvictionTimeout.
	// 2. nodeStatusUpdateFrequency needs to be large enough for kubelet to generate node
	//    status. Kubelet may fail to update node status reliably if the value is too small,
	//    as it takes time to gather all necessary node information.
	nodeStatusUpdateFrequency time.Duration

	// nodeStatusReportFrequency is the frequency that kubelet posts node
	// status to master. It is only used when node lease feature is enabled.
	nodeStatusReportFrequency time.Duration

	// lastStatusReportTime is the time when node status was last reported.
	lastStatusReportTime time.Time

	// syncNodeStatusMux is a lock on updating the node status, because this path is not thread-safe.
	// This lock is used by Kubelet.syncNodeStatus function and shouldn't be used anywhere else.
	syncNodeStatusMux sync.Mutex

	// updatePodCIDRMux is a lock on updating pod CIDR, because this path is not thread-safe.
	// This lock is used by Kubelet.syncNodeStatus function and shouldn't be used anywhere else.
	updatePodCIDRMux sync.Mutex

	// updateRuntimeMux is a lock on updating runtime, because this path is not thread-safe.
	// This lock is used by Kubelet.updateRuntimeUp function and shouldn't be used anywhere else.
	updateRuntimeMux sync.Mutex

	// nodeLeaseController claims and renews the node lease for this Kubelet
	nodeLeaseController nodelease.Controller

	// Generates pod events.
	pleg pleg.PodLifecycleEventGenerator

	// Store kubecontainer.PodStatus for all pods.
	podCache kubecontainer.Cache

	// os is a facade for various syscalls that need to be mocked during testing.
	os kubecontainer.OSInterface

	// Watcher of out of memory events.
	oomWatcher oomwatcher.Watcher

	// Monitor resource usage
	resourceAnalyzer serverstats.ResourceAnalyzer

	// Whether or not we should have the QOS cgroup hierarchy for resource management
	cgroupsPerQOS bool

	// If non-empty, pass this to the container runtime as the root cgroup.
	cgroupRoot string

	// Mounter to use for volumes.
	mounter mount.Interface

	// hostutil to interact with filesystems
	hostutil hostutil.HostUtils

	// subpather to execute subpath actions
	subpather subpath.Interface

	// Manager of non-Runtime containers.
	containerManager cm.ContainerManager

	// Maximum Number of Pods which can be run by this Kubelet
	maxPods int

	// Monitor Kubelet's sync loop
	syncLoopMonitor atomic.Value

	// Container restart Backoff
	backOff *flowcontrol.Backoff

	// Channel for sending pods to kill.
	podKillingCh chan *kubecontainer.PodPair

	// Information about the ports which are opened by daemons on Node running this Kubelet server.
	daemonEndpoints *v1.NodeDaemonEndpoints

	// A queue used to trigger pod workers.
	workQueue queue.WorkQueue

	// oneTimeInitializer is used to initialize modules that are dependent on the runtime to be up.
	oneTimeInitializer sync.Once

	// If non-nil, use this IP address for the node
	nodeIP net.IP

	// use this function to validate the kubelet nodeIP
	nodeIPValidator func(net.IP) error

	// If non-nil, this is a unique identifier for the node in an external database, eg. cloudprovider
	providerID string

	// clock is an interface that provides time related functionality in a way that makes it
	// easy to test the code.
	clock clock.Clock

	// handlers called during the tryUpdateNodeStatus cycle
	setNodeStatusFuncs []func(*v1.Node) error

	lastNodeUnschedulableLock sync.Mutex
	// maintains Node.Spec.Unschedulable value from previous run of tryUpdateNodeStatus()
	lastNodeUnschedulable bool

	// TODO: think about moving this to be centralized in PodWorkers in follow-on.
	// the list of handlers to call during pod admission.
	admitHandlers lifecycle.PodAdmitHandlers

	// softAdmithandlers are applied to the pod after it is admitted by the Kubelet, but before it is
	// run. A pod rejected by a softAdmitHandler will be left in a Pending state indefinitely. If a
	// rejected pod should not be recreated, or the scheduler is not aware of the rejection rule, the
	// admission rule should be applied by a softAdmitHandler.
	softAdmitHandlers lifecycle.PodAdmitHandlers

	// the list of handlers to call during pod sync loop.
	lifecycle.PodSyncLoopHandlers

	// the list of handlers to call during pod sync.
	lifecycle.PodSyncHandlers

	// the number of allowed pods per core
	podsPerCore int

	// enableControllerAttachDetach indicates the Attach/Detach controller
	// should manage attachment/detachment of volumes scheduled to this node,
	// and disable kubelet from executing any attach/detach operations
	enableControllerAttachDetach bool

	// trigger deleting containers in a pod
	containerDeletor *podContainerDeletor

	// config iptables util rules
	makeIPTablesUtilChains bool

	// The bit of the fwmark space to mark packets for SNAT.
	iptablesMasqueradeBit int

	// The bit of the fwmark space to mark packets for dropping.
	iptablesDropBit int

	// The AppArmor validator for checking whether AppArmor is supported.
	appArmorValidator apparmor.Validator

	// The handler serving CRI streaming calls (exec/attach/port-forward).
	criHandler http.Handler

	// experimentalHostUserNamespaceDefaulting sets userns=true when users request host namespaces (pid, ipc, net),
	// are using non-namespaced capabilities (mknod, sys_time, sys_module), the pod contains a privileged container,
	// or using host path volumes.
	// This should only be enabled when the container runtime is performing user remapping AND if the
	// experimental behavior is desired.
	experimentalHostUserNamespaceDefaulting bool

	// dockerLegacyService contains some legacy methods for backward compatibility.
	// It should be set only when docker is using non json-file logging driver.
	dockerLegacyService dockershim.DockerLegacyService

	// StatsProvider provides the node and the container stats.
	*stats.StatsProvider

	// This flag, if set, instructs the kubelet to keep volumes from terminated pods mounted to the node.
	// This can be useful for debugging volume related issues.
	keepTerminatedPodVolumes bool // DEPRECATED

	// pluginmanager runs a set of asynchronous loops that figure out which
	// plugins need to be registered/unregistered based on this node and makes it so.
	pluginManager pluginmanager.PluginManager

	// This flag sets a maximum number of images to report in the node status.
	nodeStatusMaxImages int32

	// Handles RuntimeClass objects for the Kubelet.
	runtimeClassManager *runtimeclass.Manager
}

// setupDataDirs creates:
// 1.  the root directory
// 2.  the pods directory
// 3.  the plugins directory
// 4.  the pod-resources directory
func (kl *Kubelet) setupDataDirs() error {
	kl.rootDirectory = path.Clean(kl.rootDirectory)
	// 返回rootDirectory加plugins_registry，默认/var/lib/kubelet/plugins_registry
	pluginRegistrationDir := kl.getPluginsRegistrationDir()
	// 返回rootDirectory加plugin，默认/var/lib/kubelet/plugin
	pluginsDir := kl.getPluginsDir()
	// 创建rootDirectory目录
	if err := os.MkdirAll(kl.getRootDir(), 0750); err != nil {
		return fmt.Errorf("error creating root directory: %v", err)
	}
	// 找到rootDirectory所在挂载目录（rootDirectory或rootDirectory的前缀），如果读取/proc/self/mountinfo里mount option里没有shared
	// 则执行rootDirectory的bind和rshared挂载，比如mount --bind /var/lib/kubelet /var/lib/kubelet mount --make-rshared /var/lib/kubelet
	// 一般挂载都会有shared，所以不会执行bind和rshared挂载
	if err := kl.hostutil.MakeRShared(kl.getRootDir()); err != nil {
		return fmt.Errorf("error configuring root directory: %v", err)
	}
	// 创建pods目录，默认为/var/lib/kubelet/pods
	if err := os.MkdirAll(kl.getPodsDir(), 0750); err != nil {
		return fmt.Errorf("error creating pods directory: %v", err)
	}
	// 创建plugin目录，默认为/var/lib/kubelet/plugin
	if err := os.MkdirAll(kl.getPluginsDir(), 0750); err != nil {
		return fmt.Errorf("error creating plugins directory: %v", err)
	}
	// 创建plugins_registry目录，默认为/var/lib/kubelet/plugins_registry
	if err := os.MkdirAll(kl.getPluginsRegistrationDir(), 0750); err != nil {
		return fmt.Errorf("error creating plugins registry directory: %v", err)
	}
	// 创建pod-resources目录，默认为/var/lib/kubelet/pod-resources
	if err := os.MkdirAll(kl.getPodResourcesDir(), 0750); err != nil {
		return fmt.Errorf("error creating podresources directory: %v", err)
	}
	if selinux.SELinuxEnabled() {
		err := selinux.SetFileLabel(pluginRegistrationDir, config.KubeletPluginsDirSELinuxLabel)
		if err != nil {
			klog.Warningf("Unprivileged containerized plugins might not work. Could not set selinux context on %s: %v", pluginRegistrationDir, err)
		}
		err = selinux.SetFileLabel(pluginsDir, config.KubeletPluginsDirSELinuxLabel)
		if err != nil {
			klog.Warningf("Unprivileged containerized plugins might not work. Could not set selinux context on %s: %v", pluginsDir, err)
		}
	}
	return nil
}

// StartGarbageCollection starts garbage collection threads.
func (kl *Kubelet) StartGarbageCollection() {
	loggedContainerGCFailure := false
	go wait.Until(func() {
		if err := kl.containerGC.GarbageCollect(); err != nil {
			klog.Errorf("Container garbage collection failed: %v", err)
			kl.recorder.Eventf(kl.nodeRef, v1.EventTypeWarning, events.ContainerGCFailed, err.Error())
			loggedContainerGCFailure = true
		} else {
			var vLevel klog.Level = 4
			if loggedContainerGCFailure {
				vLevel = 1
				loggedContainerGCFailure = false
			}

			klog.V(vLevel).Infof("Container garbage collection succeeded")
		}
	}, ContainerGCPeriod, wait.NeverStop)

	// when the high threshold is set to 100, stub the image GC manager
	if kl.kubeletConfiguration.ImageGCHighThresholdPercent == 100 {
		klog.V(2).Infof("ImageGCHighThresholdPercent is set 100, Disable image GC")
		return
	}

	prevImageGCFailed := false
	go wait.Until(func() {
		if err := kl.imageManager.GarbageCollect(); err != nil {
			if prevImageGCFailed {
				klog.Errorf("Image garbage collection failed multiple times in a row: %v", err)
				// Only create an event for repeated failures
				kl.recorder.Eventf(kl.nodeRef, v1.EventTypeWarning, events.ImageGCFailed, err.Error())
			} else {
				klog.Errorf("Image garbage collection failed once. Stats initialization may not have completed yet: %v", err)
			}
			prevImageGCFailed = true
		} else {
			var vLevel klog.Level = 4
			if prevImageGCFailed {
				vLevel = 1
				prevImageGCFailed = false
			}

			klog.V(vLevel).Infof("Image garbage collection succeeded")
		}
	}, ImageGCPeriod, wait.NeverStop)
}

// initializeModules will initialize internal modules that do not require the container runtime to be up.
// Note that the modules here must not depend on modules that are not initialized here.
func (kl *Kubelet) initializeModules() error {
	// Prometheus metrics.
	metrics.Register(
		kl.runtimeCache,
		collectors.NewVolumeStatsCollector(kl),
		collectors.NewLogMetricsCollector(kl.StatsProvider.ListPodStats),
	)
	metrics.SetNodeName(kl.nodeName)
	servermetrics.Register()

	// Setup filesystem directories.
	if err := kl.setupDataDirs(); err != nil {
		return err
	}

	// If the container logs directory does not exist, create it.
	// 创建/var/log/containers目录
	if _, err := os.Stat(ContainerLogsDir); err != nil {
		if err := kl.os.MkdirAll(ContainerLogsDir, 0755); err != nil {
			klog.Errorf("Failed to create directory %q: %v", ContainerLogsDir, err)
		}
	}

	// Start the image manager.
	// 每5分钟同步正在使用的镜像保存在kl.imageManager.imageRecords
	// 每30s保存node节点上所有镜像列表到kl.imageManager.imageCache.images
	kl.imageManager.Start()

	// Start the certificate manager if it was enabled.
	if kl.serverCertificateManager != nil {
		// todo read
		kl.serverCertificateManager.Start()
	}

	// Start out of memory watcher.
	// watch /dev/kmsg(监听内核消息), 当系统发生oom事件发送event消息
	if err := kl.oomWatcher.Start(kl.nodeRef); err != nil {
		return fmt.Errorf("failed to start OOM watcher %v", err)
	}

	// Start resource analyzer
	// 启动一个goroutine，周期性更新ra.fsResourceAnalyzer.cachedVolumeStats
	// ra.fsResourceAnalyzer.cachedVolumeStats保存了volumeStatCalculator集合，用于计算kubelet上所有pod的所有volume状态，volumeStatCalculator用于生成并保存pod的volume状态。
	// 每个pod会启动一个goroutine，周期性获得pod各个volume的目录使用量，inode使用量，文件系统的available bytes, byte capacity,total inodes, inodes free，并将状态保存到volumeStatCalculator.latest
	kl.resourceAnalyzer.Start()

	return nil
}

// initializeRuntimeDependentModules will initialize internal modules that require the container runtime to be up.
func (kl *Kubelet) initializeRuntimeDependentModules() {
	//  watch /dev/kmsg里的oom事件，将事件保存在cadvisor内部eventStore中
	// 监听cgroup的所有子系统目录的变化，每个子目录是一个container，根据目录的变化，生成container或移除container。kubelet里能够处理"/kubepods.slice", "/system.slice/kubelet.service", "/system.slice/docker.service"下的所有子目录，包括自身
	// 生成container会启动一个goroutine，读取cgroup子系统的文件生成监控信息保存在cadvisor内部的containers字段中。移除container则停止goroutine，在cadvisor内部的containers字段中移除这个容器的监控数据
	// 默认每5分钟执行更新machineInfo（node节点的所有属性，比如cpu、内存、磁盘、操作系统、内核等），kl.updateNodeStatus更新node state会用到
	if err := kl.cadvisor.Start(); err != nil {
		// Fail kubelet and rely on the babysitter to retry starting kubelet.
		// TODO(random-liu): Add backoff logic in the babysitter
		klog.Fatalf("Failed to start cAdvisor %v", err)
	}

	// trigger on-demand stats collection once so that we have capacity information for ephemeral storage.
	// ignore any errors, since if stats collection is not successful, the container manager will fail to start below.
	// 等待container的housekeeping（更新监控数据）完成后，返回容器最近的cpu和memory的使用情况，Accelerators状态（没有gpu）、UserDefinedMetrics（kubelet没有）、最近的网卡状态
	// 这里传入true，只是为了等待container的housekeeping（更新监控数据）完成，保证下面执行kl.containerManager.Start不会报错
	kl.StatsProvider.GetCgroupStats("/", true)
	// Start container manager.
	// 如果先从apiserver get node，不存在则创建node
	node, err := kl.getNodeAnyWay()
	if err != nil {
		// Fail kubelet and rely on the babysitter to retry starting kubelet.
		klog.Fatalf("Kubelet failed to get node info: %v", err)
	}
	// containerManager must start after cAdvisor because it needs filesystem capacity information
	// 启动cpu manager
	//   1. 进行容器的垃圾回收，不存在的容器分配的cpu回收（在checkpoint和stateMemory中回收），containerMap列表清理（让容器的cpu分配状态里容器与activePods一致）
	//   2. 创建checkpoint和stateMemory来保存当前的分配策略、分配状态
	//   3. 如果cpu manager的policy为static policy会校验当前分配状态是否合法
	//   3.1 创建一个goroutine周期性（默认10s）周期同步m.state中容器的cpuset到容器的cgroup设置，会同步activepods返回的pod的容器集合，清理不存在的容器的cpu分配
	// 校验（cpu、内存、hugepage、ephemeral-storage）资源的保留值是否大于kubelet自身的能力
	// 维护cgroup目录
	//   1. 检查系统里cgroup是否开启"cpu"、"cpuacct", "cpuset", "memory"子系统，cpu子系统开启cfs
	//   2. 设置一些关键的sysctl项
	//   3. 确保各个cgroup子系统挂载目录下kubepods或kubepods.slice文件夹存在
	//      设置各个cgroup系统（memory、pid、cpu share、hugepage）的属性值--这个值是根据cm.internalCapacity列表，各个资源类型的值减去SystemReserved和KubeReserved
	//   4.启动qosmanager
	//        确保Burstable和BestEffort的cgroup目录存在，Burstable目录为/sys/fs/cgroup/{cgroup system}/kubepods.slice/burstable，BestEffort目录为/sys/fs/cgroup/{cgroup system}/kubepods.slice/besteffort
	//        启动一个goroutine 每一份钟更新Burstable和BestEffort的cgroup目录的cgroup资源属性
	//   5. 应用--enforce-node-allocatable的配置，设置各个（cpu、memory、pid、hugepage）cgroup的限制值
	//      为pods，则设置在/sys/fs/cgroup/{cgroup sub system}/kubepods.slice，限制值为各个类型capacity减去SystemReserved和KubeReserved
	//      为system-reserved， 则设置在/sys/fs/cgroup/{cgroup sub system}/{basename system-reserved-cgroup}，限制值为各个类型system-reserved值
	//      为kube-reserved， 则设置在/sys/fs/cgroup/{cgroup sub system}/{basename kube-reserved-cgroup}，限制值为各个类型kube-reserved
	// 执行周期性任务
	//   1. 启动一个goroutine，每一分钟执行cm.systemContainers的ensureStateFunc
	//      当cm.SystemCgroupsName不为空， cm.SystemCgroupsName: newSystemCgroups(cm.SystemCgroupsName), ensureStateFunc: 将非内核pid或非init进程的pid移动到cm.SystemCgroupsNamecgroup中
	//      当cm.kubeletCgroupsName不为空，cm.kubeletCgroupsName: newSystemCgroups(cm.KubeletCgroupsName), ensureStateFunc: 设置kubelet进程的oom_score_adj为-999,将kubelet进程移动到kubeletCgroupsName的cgroup路径中
	//   2. 启动一个goroutine，每5分钟执行cm.periodicTasks里的task
	//     当运行时为docker，设置cm.RuntimeCgroupsName为docker的cgroup路径
	//     当cm.KubeletCgroupsName为空（没有配置kubelet cgroup），设置kubelet进程的oom_score_adj为-999
	// 启动device manager
	//   1. 读取/var/lib/kubelet/device-plugins/kubelet_internal_checkpoint文件，恢复状态
	//   2. 移除/var/lib/kubelet/device-plugins/文件夹下非"kubelet_internal_checkpoint"文件
	//   3. 启动grpc服务，监听socket为/var/lib/kubelet/device-plugins/kubelet.socket
	if err := kl.containerManager.Start(node, kl.GetActivePods, kl.sourcesReady, kl.statusManager, kl.runtimeService); err != nil {
		// Fail kubelet and rely on the babysitter to retry starting kubelet.
		klog.Fatalf("Failed to start ContainerManager %v", err)
	}
	// eviction manager must start after cadvisor because it needs to know if the container runtime has a dedicated imagefs
	// 1. 如果启用KernelMemcgNotification，则为"memory.available"或"allocatableMemory.available"阈值类型，各创建一个notifier，消费notifier.events里的消息，添加notifier到kl.evictionManager.thresholdNotifiers，执行第二个步骤
	// 2. 启动一个goroutine，每隔10s时间
	// 2.1. 执行检测是否达到驱逐阈值，执行pod的驱逐。完成驱逐后，等待30s，每1s执行检测所有pod资源是否回收完毕，如果都回收完毕则直接返回。
	// 2.2. 如果启用KernelMemcgNotification，每个notifier，停止老的goroutine并启动一个goroutine，利用epoll和cgroup目录（/sys/fs/cgroup/memory或/sys/fs/cgroup/memory/kubepods.slice）下cgroup.event_control，循环每次等待10s时间，如果有期望事件发生，则发送消息给kl.evictionManager.thresholdNotifiers[*].events
	// 2.3. memory阈值类型是"allocatableMemory.available"，监听"/sys/fs/cgroup/memory/kubepods.slice"下"memory.usage_in_bytes"文件
	// 2.4. memory阈值类型是"memory.available"，监听"/sys/fs/cgroup/memory"下"memory.usage_in_bytes"文件
	kl.evictionManager.Start(kl.StatsProvider, kl.GetActivePods, kl.podResourcesAreReclaimed, evictionMonitoringPeriod)

	// container log manager must start after container runtime is up to retrieve information from container runtime
	// and inform container to reopen log file after log rotation.
	// runtime是dockershim不做任何事情
	// TODORead
	kl.containerLogManager.Start()
	// Adding Registration Callback function for CSI Driver
	kl.pluginManager.AddHandler(pluginwatcherapi.CSIPlugin, plugincache.PluginHandler(csi.PluginHandler))
	// Adding Registration Callback function for Device Manager
	kl.pluginManager.AddHandler(pluginwatcherapi.DevicePlugin, kl.containerManager.GetPluginRegistrationHandler())
	// Start the plugin manager
	klog.V(4).Infof("starting plugin manager")
	// TODORead
	go kl.pluginManager.Run(kl.sourcesReady, wait.NeverStop)
}

// Run starts the kubelet reacting to config updates
func (kl *Kubelet) Run(updates <-chan kubetypes.PodUpdate) {
	if kl.logServer == nil {
		// 提供访问容器日志
		kl.logServer = http.StripPrefix("/logs/", http.FileServer(http.Dir("/var/log/")))
	}
	if kl.kubeClient == nil {
		klog.Warning("No api server defined - no node status update will be sent.")
	}

	// Start the cloud provider sync manager
	// 启动一个goroutine 每10s从cloudprovider同步一下ip地址
	if kl.cloudResourceSyncManager != nil {
		go kl.cloudResourceSyncManager.Run(wait.NeverStop)
	}

	// 1. 注册metric
	// 2. 创建kubelet数据目录（pods、plugins、pod-resources）
	// 3. 创建/var/log/containers目录
	// 4. 启动image manager
	// 5. 启动certificate manager，如果启用证书轮转
	// 6. 启动oomWatcher，watch /dev/kmsg(监听内核消息), 当系统发生oom事件发送event消息
	// 7. 启动资源状态监控
	if err := kl.initializeModules(); err != nil {
		kl.recorder.Eventf(kl.nodeRef, v1.EventTypeWarning, events.KubeletSetupFailed, err.Error())
		klog.Fatal(err)
	}

	// Start volume manager
	// todo read
	go kl.volumeManager.Run(kl.sourcesReady, wait.NeverStop)

	if kl.kubeClient != nil {
		// Start syncing node status immediately, this may set up things the runtime needs to run.
		//周期性的更新node的status，包括capabilities，镜像、cidr等
		// nodeStatusUpdateFrequency默认为10s
		go wait.Until(kl.syncNodeStatus, kl.nodeStatusUpdateFrequency, wait.NeverStop)
		//这里面也会调用kl.updateRuntimeUp和kl.syncNodeStatus()，只执行一次
		go kl.fastStatusUpdateOnce()

		// start syncing lease
		// 默认为每10秒更新lease
		go kl.nodeLeaseController.Run(wait.NeverStop)
	}
	// 调用cri接口的Status查询 runtime status--包括RuntimeReady、NetworkReady
	// 设置kl.runtimeState，kl.syncNodeStatus()会通过kl.runtimeState.runtimeErrors()和kl.runtimeState.networkErrors()和kl.runtimeState.storageErrors()检测runtime状态
	go wait.Until(kl.updateRuntimeUp, 5*time.Second, wait.NeverStop)

	// Set up iptables util rules
	if kl.makeIPTablesUtilChains {
		kl.initNetworkUtil()
	}

	// Start a goroutine responsible for killing pods (that are not properly
	// handled by pod workers).
	// 从kl.podKillingCh取出一条pod删除消息，判断是否重复消息。非重复消息，则启动一个goroutine，执行kl.killPod
	// 1. 停止pod里所有的container，每个container都启动一个goroutine进行killContainer（执行prestop和stop container）
	// 2. 调用网络插件释放容器的网卡，顺序停止pod里所有的sandbox container
	// 3. 更新Burstable和BestEffort qos class的cpu、memory、hugepage的cgroup目录属性值
	go wait.Until(kl.podKiller, 1*time.Second, wait.NeverStop)

	// Start component sync loops.
	kl.statusManager.Start()
	// 启动一个goroutine，监听所有readiness probe的结果发生变化
	// kl.probeManager.readinessManager.updates通道中读取消息
	// 消息中（readiness是否成功结果）跟kl.probeManager.statusManager里的container status里的Ready字段值一样，则不做任何东西
	// 否则，同步readiness结果到kl.statusManager.podStatuses，并更新apiserver中pod status

	// 启动一个goroutine，监听所有start probe的结果发生变化
	// 从kl.probeManager.startupManager.updates通道中读取消息
	// 消息中startup probe是否成功结果，跟kl.statusManager里的container status里的Started字段值一样，则不做任何东西
	// 否则，更改container status里Started字段的值（同步startup probe结果）到kl.statusManager.podStatuses，并更新apiserver中pod status

	// liveness probe结果在在kl.syncLoopIteration里被消费
	kl.probeManager.Start()

	// Start syncing RuntimeClasses if enabled.
	// 启动runtimeClass informer
	if kl.runtimeClassManager != nil {
		kl.runtimeClassManager.Start(wait.NeverStop)
	}

	// Start the pod lifecycle event generator.
	// 从runtime中获得所有运行和不在运行的pod
	// 将pods更新到kl.pleg.podRecords里每个uid的current
	// 遍历kl.pleg.podRecords里的pod，遍历pod里的普通container和sandbox，根据老的和新的container状态生成PodLifecycleEvent
	// 遍历所有PodLifecycleEvent：
	//    如果启用podStatus缓存，则更新pod的podStatus缓存
	//    更新pid的kl.pleg.podRecords：
	//       id不在podRecords中，直接返回
	//       如果kl.pleg.podRecords.current为nil，则从podRecords中移除这个id（因为pod已经被删除了），直接返回
	//       否则，将kl.pleg.podRecords.old设置成kl.pleg.podRecords.current，kl.pleg.podRecords.current置为nil
	//   发送event到kl.pleg.eventChannel，如果kl.pleg.eventChannel满了，则丢弃这个event
	kl.pleg.Start()
	kl.syncLoop(updates, kl)
}

// syncPod is the transaction script for the sync of a single pod.
//
// Arguments:
//
// o - the SyncPodOptions for this invocation
//
// The workflow is:
// * If the pod is being created, record pod worker start latency
// * Call generateAPIPodStatus to prepare an v1.PodStatus for the pod
// * If the pod is being seen as running for the first time, record pod
//   start latency
// * Update the status of the pod in the status manager
// * Kill the pod if it should not be running
// * Create a mirror pod if the pod is a static pod, and does not
//   already have a mirror pod
// * Create the data directories for the pod if they do not exist
// * Wait for volumes to attach/mount
// * Fetch the pull secrets for the pod
// * Call the container runtime's SyncPod callback
// * Update the traffic shaping for the pod's ingress and egress limits
//
// If any step of this workflow errors, the error is returned, and is repeated
// on the next syncPod call.
//
// This operation writes all events that are dispatched in order to provide
// the most accurate information possible about an error situation to aid debugging.
// Callers should not throw an event if this operation returns an error.
func (kl *Kubelet) syncPod(o syncPodOptions) error {
	// pull out the required options
	pod := o.pod
	mirrorPod := o.mirrorPod
	podStatus := o.podStatus
	updateType := o.updateType

	// if we want to kill a pod, do it now!
	// evictionManager进行驱逐pod的类型为kubetypes.SyncPodKill
	if updateType == kubetypes.SyncPodKill {
		killPodOptions := o.killPodOptions
		if killPodOptions == nil || killPodOptions.PodStatusFunc == nil {
			return fmt.Errorf("kill pod options are required if update type is kill")
		}
		// 在pkg\kubelet\pod_workers.go里killPodNow里调用到这里，返回是pod的status（v1.PodStatus类型）
		apiPodStatus := killPodOptions.PodStatusFunc(pod, podStatus)
		// 将apiPodStatus规整化后，与内部的m.podStatuses进行比较，发生变化，则发送到m.podStatusChannel进行更新或等待批量更新
		kl.statusManager.SetPodStatus(pod, apiPodStatus)
		// we kill the pod with the specified grace period since this is a termination
		// 从kl.Podkiller()调用这个，则gracePeriodOverride为nil
		// 1. 停止pod里所有的container，每个container都启动一个goroutine进行killContainer（执行prestop和stop container）
		// 2. 调用网络插件释放容器的网卡，顺序停止pod里所有的sandbox container
		// 3. 更新Burstable和BestEffort qos class的cpu、memory、hugepage的cgroup目录属性值
		if err := kl.killPod(pod, nil, podStatus, killPodOptions.PodTerminationGracePeriodSecondsOverride); err != nil {
			kl.recorder.Eventf(pod, v1.EventTypeWarning, events.FailedToKillPod, "error killing pod: %v", err)
			// there was an error killing the pod, so we return that error directly
			utilruntime.HandleError(err)
			return err
		}
		return nil
	}

	// Latency measurements for the main workflow are relative to the
	// first time the pod was seen by the API server.
	var firstSeenTime time.Time
	if firstSeenTimeStr, ok := pod.Annotations[kubetypes.ConfigFirstSeenAnnotationKey]; ok {
		firstSeenTime = kubetypes.ConvertToTimestamp(firstSeenTimeStr).Get()
	}

	// Record pod worker start latency if being created
	// TODO: make pod workers record their own latencies
	if updateType == kubetypes.SyncPodCreate {
		if !firstSeenTime.IsZero() {
			// This is the first time we are syncing the pod. Record the latency
			// since kubelet first saw the pod if firstSeenTime is set.
			metrics.PodWorkerStartDuration.Observe(metrics.SinceInSeconds(firstSeenTime))
		} else {
			klog.V(3).Infof("First seen time not recorded for pod %q", pod.UID)
		}
	}

	// Generate final API pod status with pod and status manager status
	// 根据runtime的status生成pod status
	apiPodStatus := kl.generateAPIPodStatus(pod, podStatus)
	// The pod IP may be changed in generateAPIPodStatus if the pod is using host network. (See #24576)
	// TODO(random-liu): After writing pod spec into container labels, check whether pod is using host network, and
	// set pod IP to hostIP directly in runtime.GetPodStatus
	podStatus.IPs = make([]string, 0, len(apiPodStatus.PodIPs))
	for _, ipInfo := range apiPodStatus.PodIPs {
		podStatus.IPs = append(podStatus.IPs, ipInfo.IP)
	}

	if len(podStatus.IPs) == 0 && len(apiPodStatus.PodIP) > 0 {
		podStatus.IPs = []string{apiPodStatus.PodIP}
	}

	// Record the time it takes for the pod to become running.
	// 从m.podStatuses缓存中获取uid的相关的pod的status，（如果是mirror pod uid则通过static pod uid来查找）
	existingStatus, ok := kl.statusManager.GetPodStatus(pod.UID)
	// status manager中没有pod状态，或status manager中pod状态为"Pending"且现在的状态为"Running"且pod的annotation中"kubernetes.io/config.seen"不为0，则记录pod变成running的时间
	if !ok || existingStatus.Phase == v1.PodPending && apiPodStatus.Phase == v1.PodRunning &&
		!firstSeenTime.IsZero() {
		metrics.PodStartDuration.Observe(metrics.SinceInSeconds(firstSeenTime))
	}

	// kl.softAdmitHandlers里的handler是否都admit通过（appArmor和NoNewPrivs和ProcMount）
	runnable := kl.canRunPod(pod)
	// admit不通过
	if !runnable.Admit {
		// Pod is not runnable; update the Pod and Container statuses to why.
		// 设置pod的Reason和Message
		apiPodStatus.Reason = runnable.Reason
		apiPodStatus.Message = runnable.Message
		// Waiting containers are not creating.
		const waitingReason = "Blocked"
		// 当initcontainer处于Waiting状态，设置container的status的Reason为"Blocked"
		for _, cs := range apiPodStatus.InitContainerStatuses {
			if cs.State.Waiting != nil {
				cs.State.Waiting.Reason = waitingReason
			}
		}
		// 当普通Container处于Waiting状态，设置container的status的Reason为"Blocked"
		for _, cs := range apiPodStatus.ContainerStatuses {
			if cs.State.Waiting != nil {
				cs.State.Waiting.Reason = waitingReason
			}
		}
	}

	// Update status in the status manager
	// 将status规整化后，与内部的m.podStatuses进行比较，发生变化，则发送到m.podStatusChannel进行更新或等待批量更新
	kl.statusManager.SetPodStatus(pod, apiPodStatus)

	// Kill pod if it should not be running
	// admit不通过或pod被删除或pod的phase为"Failed"（包括在kl.generateAPIPodStatus里，处理超过pod.Spec.ActiveDeadlineSeconds，将Phase改为"Failed"），则执行pod的停止操作，然后返回syncErr错误
	if !runnable.Admit || pod.DeletionTimestamp != nil || apiPodStatus.Phase == v1.PodFailed {
		var syncErr error
		// 1. 停止pod里所有的container，每个container都启动一个goroutine进行killContainer（执行prestop和stop container）
		// 2. 调用网络插件释放容器的网卡，顺序停止pod里所有的sandbox container
		// 3. 更新Burstable和BestEffort qos class的cpu、memory、hugepage的cgroup目录属性值
		if err := kl.killPod(pod, nil, podStatus, nil); err != nil {
			kl.recorder.Eventf(pod, v1.EventTypeWarning, events.FailedToKillPod, "error killing pod: %v", err)
			syncErr = fmt.Errorf("error killing pod: %v", err)
			utilruntime.HandleError(syncErr)
		} else {
			if !runnable.Admit {
				// There was no error killing the pod, but the pod cannot be run.
				// Return an error to signal that the sync loop should back off.
				syncErr = fmt.Errorf("pod cannot be run: %s", runnable.Message)
			}
		}
		return syncErr
	}

	// If the network plugin is not ready, only start the pod if it uses the host network
	// 如果网络插件unready，则只运行host网络模式的pod。这个解决了鸡生蛋蛋生鸡问题（网络插件是daemonset运行）
	if err := kl.runtimeState.networkErrors(); err != nil && !kubecontainer.IsHostNetworkPod(pod) {
		kl.recorder.Eventf(pod, v1.EventTypeWarning, events.NetworkNotReady, "%s: %v", NetworkNotReadyErrorMsg, err)
		return fmt.Errorf("%s: %v", NetworkNotReadyErrorMsg, err)
	}

	// Create Cgroups for the pod and apply resource parameters
	// to them if cgroups-per-qos flag is enabled.
	pcm := kl.containerManager.NewPodContainerManager()
	// If pod has already been terminated then we need not create
	// or update the pod's cgroup
	// pod的Phase不为"Failed"和"Succeeded"，或pod没有被删除，或pod被删除但是还有container处于running
	if !kl.podIsTerminated(pod) {
		// When the kubelet is restarted with the cgroups-per-qos
		// flag enabled, all the pod's running containers
		// should be killed intermittently and brought back up
		// under the qos cgroup hierarchy.
		// Check if this is the pod's first sync
		// 是否第一次执行
		firstSync := true
		for _, containerStatus := range apiPodStatus.ContainerStatuses {
			// 有容器在运行，则不是第一次执行
			if containerStatus.State.Running != nil {
				firstSync = false
				break
			}
		}
		// Don't kill containers in pod if pod's cgroups already
		// exists or the pod is running for the first time
		podKilled := false
		// 至少有一个cgroup子系统中，pod的cgroup路径不存在，且不是第一次执行，则执行pod停止操作
		if !pcm.Exists(pod) && !firstSync {
			// 1. 停止pod里所有的container，每个container都启动一个goroutine进行killContainer（执行prestop和stop container）
			// 2. 调用网络插件释放容器的网卡，顺序停止pod里所有的sandbox container
			// 3. 更新Burstable和BestEffort qos class的cpu、memory、hugepage的cgroup目录属性值
			if err := kl.killPod(pod, nil, podStatus, nil); err == nil {
				podKilled = true
			}
		}
		// Create and Update pod's Cgroups
		// Don't create cgroups for run once pod if it was killed above
		// The current policy is not to restart the run once pods when
		// the kubelet is restarted with the new flag as run once pods are
		// expected to run only once and if the kubelet is restarted then
		// they are not expected to run again.
		// We don't create and apply updates to cgroup if its a run once pod and was killed above
		// pod没有被停止（第一次执行，或不是第一次执行且cgroup至少一个存在），或pod的RestartPolicy不为"Never"
		// 且至少有一个cgroup子系统中，pod的cgroup路径不存在，则创建cgroup目录和设置cgroup属性
		if !(podKilled && pod.Spec.RestartPolicy == v1.RestartPolicyNever) {
			// 至少有一个cgroup子系统中，pod的cgroup路径不存在
			if !pcm.Exists(pod) {
				// active pod是所有pods中（普通pod和static pod），pod的Phase不是"Failed"和"Succeeded"，或pod没有被删除或至少一个container为running
				// 根据所有active pod来统计Burstable和BestEffort的cgroup属性
				// 设置Burstable和BestEffort qos class的cpu、memory、hugepage的cgroup属性值
				if err := kl.containerManager.UpdateQOSCgroups(); err != nil {
					klog.V(2).Infof("Failed to update QoS cgroups while syncing pod: %v", err)
				}
				// 在所有cgroup子系统，创建pod的cgroup路径，并设置相应cgroup（cpu、memory、pid、hugepage在系统中开启的）资源属性的限制
				if err := pcm.EnsureExists(pod); err != nil {
					kl.recorder.Eventf(pod, v1.EventTypeWarning, events.FailedToCreatePodContainer, "unable to ensure pod container exists: %v", err)
					return fmt.Errorf("failed to ensure that the pod: %v cgroups exist and are correctly applied: %v", pod.UID, err)
				}
			}
		}
	}

	// Create Mirror Pod for Static Pod if it doesn't already exist
	// 如果是static pod，则检查是否存在mirror pod，当mirror pod不存在时候进行创建
	if kubetypes.IsStaticPod(pod) {
		// pod.Name + "_" + pod.Namespace
		podFullName := kubecontainer.GetPodFullName(pod)
		deleted := false
		if mirrorPod != nil {
			// mirrorPod被删除或mirrorPod不是pod的mirror pod
			if mirrorPod.DeletionTimestamp != nil || !kl.podManager.IsMirrorPodOf(mirrorPod, pod) {
				// The mirror pod is semantically different from the static pod. Remove
				// it. The mirror pod will get recreated later.
				klog.Infof("Trying to delete pod %s %v", podFullName, mirrorPod.ObjectMeta.UID)
				var err error
				// 调用api删除mirror pod（从podFullName解析出name和namespace）
				deleted, err = kl.podManager.DeleteMirrorPod(podFullName, &mirrorPod.ObjectMeta.UID)
				if deleted {
					klog.Warningf("Deleted mirror pod %q because it is outdated", format.Pod(mirrorPod))
				} else if err != nil {
					klog.Errorf("Failed deleting mirror pod %q: %v", format.Pod(mirrorPod), err)
				}
			}
		}
		// static pod没有mirror pod或mirror pod被删除，则在获取node未发生错误且node未被删除条件下，创建mirror pod
		if mirrorPod == nil || deleted {
			// 当kl.kubeClient为nil，从kl.initialNode手动生成的初始的node对象，否则从informer中获取本机的node对象
			node, err := kl.GetNode()
			// 当获取node发生错误或node被删除，则无需创建mirror pod
			if err != nil || node.DeletionTimestamp != nil {
				klog.V(4).Infof("No need to create a mirror pod, since node %q has been removed from the cluster", kl.nodeName)
			} else {
				klog.V(4).Infof("Creating a mirror pod for static pod %q", format.Pod(pod))
				// node没有被删除，则创建static pod的mirror pod
				if err := kl.podManager.CreateMirrorPod(pod); err != nil {
					klog.Errorf("Failed creating a mirror pod for %q: %v", format.Pod(pod), err)
				}
			}
		}
	}

	// Make data directories for the pod
	// 创建这个pod相关的目录（volumes和plugins）
	if err := kl.makePodDataDirs(pod); err != nil {
		kl.recorder.Eventf(pod, v1.EventTypeWarning, events.FailedToMakePodDataDirectories, "error making pod data directories: %v", err)
		klog.Errorf("Unable to make pod data directories for pod %q: %v", format.Pod(pod), err)
		return err
	}

	// Volume manager will not mount volumes for terminated pods
	// pod的Phase不为"Failed"和"Succeeded"，或pod没有被删除，或pod被删除但是还有container处于running
	if !kl.podIsTerminated(pod) {
		// Wait for volumes to attach/mount
		// 在2m3s内等待pod的所有volume进行挂载
		if err := kl.volumeManager.WaitForAttachAndMount(pod); err != nil {
			kl.recorder.Eventf(pod, v1.EventTypeWarning, events.FailedMountVolume, "Unable to attach or mount volumes: %v", err)
			klog.Errorf("Unable to attach or mount volumes for pod %q: %v; skipping pod", format.Pod(pod), err)
			return err
		}
	}

	// Fetch the pull secrets for the pod
	// 获取pod相关的所有ImagePullSecrets
	pullSecrets := kl.getPullSecretsForPod(pod)

	// Call the container runtime's SyncPod callback
	// 1. 根据runtime种container和sandbox的状态，计算出需要采取的动作
	// 2. 执行相关的动作，Kill pod sandbox、Kill any containers、 Create sandbox、Create ephemeral containers、Create init containers、Create normal containers
	result := kl.containerRuntime.SyncPod(pod, podStatus, pullSecrets, kl.backOff)
	// 只更新"StartContainer"的动作的事件，如果result没有错误，则移除缓存中"{uid}_{target}"相关的错误，否则就增加"{uid}_{target}"的相关错误
	kl.reasonCache.Update(pod.UID, result)
	if err := result.Error(); err != nil {
		// Do not return error if the only failures were pods in backoff
		// 包含kubecontainer.ErrCrashLoopBackOff或images.ErrImagePullBackOff错误（这两个错误会在最前面），就返回nil
		// 其他错误，就返回错误
		for _, r := range result.SyncResults {
			if r.Error != kubecontainer.ErrCrashLoopBackOff && r.Error != images.ErrImagePullBackOff {
				// Do not record an event here, as we keep all event logging for sync pod failures
				// local to container runtime so we get better errors
				return err
			}
		}

		return nil
	}

	return nil
}

// Get pods which should be resynchronized. Currently, the following pod should be resynchronized:
//   * pod whose work is ready.
//   * internal modules that request sync of a pod.
// 返回的pod包含
// 1. 这个pod uid是在pod worker中处理过的uid
// 2. pod中还未被pod worker处理完，让kl.podSyncLoopHandler决定（返回true的）加入podsToSync返回列表里
// kl.podSyncLoopHandler只有一个pkg\kubelet\active_deadline.go activeDeadlineHandler
// activeDeadlineHandler
// 检测pod status中StartTime距现在时间长度，超出pod.Spec.ActiveDeadlineSeconds，返回true
// 即pod的active时间长度是否超过pod.Spec.ActiveDeadlineSeconds
func (kl *Kubelet) getPodsToSync() []*v1.Pod {
	// 返回所有普通pod和static pod
	allPods := kl.podManager.GetPods()
	// 获得worker queue中过期的uid列表，worker queue中uid列表是在pod worker执行完之后加入的（无论是否执行出现错误）
	podUIDs := kl.workQueue.GetWork()
	podUIDSet := sets.NewString()
	for _, podUID := range podUIDs {
		podUIDSet.Insert(string(podUID))
	}
	var podsToSync []*v1.Pod
	for _, pod := range allPods {
		// 这个pod uid是在pod worker中处理过的uid
		if podUIDSet.Has(string(pod.UID)) {
			// The work of the pod is ready
			podsToSync = append(podsToSync, pod)
			continue
		}
		// pod中还未被pod worker处理完，让kl.podSyncLoopHandler决定是否
		// kl.podSyncLoopHandler只有一个pkg\kubelet\active_deadline.go activeDeadlineHandler
		// activeDeadlineHandler
		// 检测pod status中StartTime距现在时间长度，超出pod.Spec.ActiveDeadlineSeconds，返回true
		// 即pod的active时间长度是否超过pod.Spec.ActiveDeadlineSeconds
		for _, podSyncLoopHandler := range kl.PodSyncLoopHandlers {
			if podSyncLoopHandler.ShouldSync(pod) {
				podsToSync = append(podsToSync, pod)
				break
			}
		}
	}
	return podsToSync
}

// deletePod deletes the pod from the internal state of the kubelet by:
// 1.  stopping the associated pod worker asynchronously
// 2.  signaling to kill the pod by sending on the podKillingCh channel
//
// deletePod returns an error if not all sources are ready or the pod is not
// found in the runtime cache.
// kl.sourcesReady.seenSources里的所有源已经注册且都提供了至少一个pod
// 如果pod在kl.podWorkers.podUpdates有UpdatePodOptions chan，则关闭这个chan，并从kl.podWorkers.podUpdates中移除。在kl.podWorkers.lastUndeliveredWorkUpdate里有pod未处理的事件，直接删除
// 发送podPair消息给kl.podKillingCh，让kl.podKiller消费，进行pod删除（停止所有container）
func (kl *Kubelet) deletePod(pod *v1.Pod) error {
	if pod == nil {
		return fmt.Errorf("deletePod does not allow nil pod")
	}
	// kl.sourcesReady.seenSources里的不是所有源已经注册且都提供了至少一个pod
	if !kl.sourcesReady.AllReady() {
		// If the sources aren't ready, skip deletion, as we may accidentally delete pods
		// for sources that haven't reported yet.
		return fmt.Errorf("skipping delete because sources aren't ready yet")
	}
	// 如果pod在kl.podWorkers.podUpdates有UpdatePodOptions chan，则关闭这个chan，并从kl.podWorkers.podUpdates中移除。在kl.podWorkers.lastUndeliveredWorkUpdate里有pod未处理的事件，直接删除
	kl.podWorkers.ForgetWorker(pod.UID)

	// Runtime cache may not have been updated to with the pod, but it's okay
	// because the periodic cleanup routine will attempt to delete again later.
	// 缓存未过期，则返回缓存中（kl.runtimeCache.pods）的pod
	// 缓存过期了，则从runtime中获得所有的running pod，并更新kl.runtimeCache.pods和kl.runtimeCache.cacheTime
	runningPods, err := kl.runtimeCache.GetPods()
	if err != nil {
		return fmt.Errorf("error listing containers: %v", err)
	}
	// 从runningPods中，根据pod uid查找pod
	runningPod := kubecontainer.Pods(runningPods).FindPod("", pod.UID)
	if runningPod.IsEmpty() {
		return fmt.Errorf("pod not found")
	}
	podPair := kubecontainer.PodPair{APIPod: pod, RunningPod: &runningPod}

	// 发送给kl.podKillingCh，让kl.podKiller消费，进行pod删除（停止所有container）
	kl.podKillingCh <- &podPair
	// TODO: delete the mirror pod here?

	// We leave the volume/directory cleanup to the periodic cleanup routine.
	return nil
}

// rejectPod records an event about the pod with the given reason and message,
// and updates the pod to the failed phase in the status manage.
func (kl *Kubelet) rejectPod(pod *v1.Pod, reason, message string) {
	kl.recorder.Eventf(pod, v1.EventTypeWarning, reason, message)
	kl.statusManager.SetPodStatus(pod, v1.PodStatus{
		Phase:   v1.PodFailed,
		Reason:  reason,
		Message: "Pod " + message})
}

// canAdmitPod determines if a pod can be admitted, and gives a reason if it
// cannot. "pod" is new pod, while "pods" are all admitted pods
// The function returns a boolean value indicating whether the pod
// can be admitted, a brief single-word reason and a message explaining why
// the pod cannot be admitted.
func (kl *Kubelet) canAdmitPod(pods []*v1.Pod, pod *v1.Pod) (bool, string, string) {
	// the kubelet will invoke each pod admit handler in sequence
	// if any handler rejects, the pod is rejected.
	// TODO: move out of disk check into a pod admitter
	// TODO: out of resource eviction should have a pod admitter call-out
	attrs := &lifecycle.PodAdmitAttributes{Pod: pod, OtherPods: pods}
	// 通过evictionAdmitHandler admit条件（下列条件满足一个）
	//
	// 1. 所有资源没有达到evict阈值，则直接通过admit
	// 2. pod是static pod或mirror pod，或pod设置了优先级且优先级大于2000000000，则直接通过admit
	// 3. 只有一个"MemoryPressure" condition，qos不为"BestEffort"的pod，则直接通过admit
	// 4. 只有一个"MemoryPressure" condition，如果"BestEffort"的pod能够容忍taint "node.kubernetes.io/memory-pressure"、effect为"NoSchedule"，则直接通过admit
	//
	// 通过sysctlsWhitelist admit条件
	// 不是这三种情况
	// 1. sysctl的namespace group为"ipc"且设置了hostIPC共享
	// 2. sysctl的namespace group为"net"且设置了hostNet共享
	// 3. sysctl不在白名单里
	//
	// 通过AllocateResourcesPodAdmitHandler admit条件
	// 实现为containerManagerImpl.resourceAllocator
	// 1. 成功为pod里所有普通container和init container，分配container的limit中所有需要device plugins的资源，且成功分配cpu（如果policy为static policy，成功为Guaranteed的pod且container request的cpu为整数的container，分配cpu）
	// 实现为containerManagerImpl.topologyManager
	// 1. policy为"none"，行为跟pkg\kubelet\cm\container_manager_linux.go里的resourceAllocator一样
	//   成功为pod里所有普通container和init container，分配container的limit中所有需要device plugins的资源，且成功分配cpu（如果policy为static policy，成功为Guaranteed的pod且container request的cpu为整数的container，分配cpu），则admit通过
	// 2. policy为"best-effort"、"restricted"、"single-numa-node"
	//   遍历所有的hintProviders（cpu manager和device manager）生成container的各个资源的TopologyHint列表
	//   从各个资源的TopologyHint列表中挑出一个TopologyHint，进行组合成[]TopologyHint，然后跟default Affinity（亲和所有numaNode）进行与计算。
	//   在所有组合中，根据各个policy类型，挑选出最合适的TopologyHint。
	//   admit结果：
	//   policy为"best-effort"，则admit通过
	//   policy为"restricted"、"single-numa-node"，则为最合适TopologyHint的Preferred的值
	//
	// 通过lifecycle.NewPredicateAdmitHandler admit条件（且的关系）
	//
	// 1. node可以分配的资源可以满足，pod的request资源里的cpu、memory、EphemeralStorage、ScalarResources
	// 2. node匹配pod的Spec.NodeSelector和pod.Spec.Affinity
	// 3. pod.Spec.NodeName等于Node Name
	// 4. 检查pod里的host port与node上已经存在的host port不冲突
	//
	// 5. 只有node资源不满足，且pod是static pod、或mirror pod，或pod优先级大于等于2000000000
	//   根据最优抢占算法和pod qos筛选出被抢占的pod，这些被抢占pod的资源，能够满足admit pod需要
	for _, podAdmitHandler := range kl.admitHandlers {
		if result := podAdmitHandler.Admit(attrs); !result.Admit {
			return false, result.Reason, result.Message
		}
	}

	return true, "", ""
}

// kl.softAdmitHandlers里的handler是否都admit通过
func (kl *Kubelet) canRunPod(pod *v1.Pod) lifecycle.PodAdmitResult {
	attrs := &lifecycle.PodAdmitAttributes{Pod: pod}
	// Get "OtherPods". Rejected pods are failed, so only include admitted pods that are alive.
	// node上的普通pod和static pod中，过滤出还在运行的pod
	attrs.OtherPods = kl.filterOutTerminatedPods(kl.podManager.GetPods())

	// softAdmitHandlers有：
	// lifecycle.NewAppArmorAdmitHandler(klet.appArmorValidator)  appArmorAdmitHandler 验证pod的所有container的profile是合法且已加载
	// lifecycle.NewNoNewPrivsAdmitHandler(klet.containerRuntime) noNewPrivsAdmitHandler
	//   admit不通过，下面条件都要满足：
	//     1. pod处于"Pending"状态
	//     2. pod.Spec.Containers有一个container定义了SecurityContext且配置了SecurityContext.AllowPrivilegeEscalation且值为false
	//     3. runtime为docker
	//     4. docker的apiserver版本小于1.23.0
	// lifecycle.NewProcMountAdmitHandler(klet.containerRuntime)
	//    admit不通过，下面条件都要满足：
	//      1. pod处于"Pending"状态
	//      2. pod.Spec.Containers有一个container定义了SecurityContext且配置了SecurityContext.ProcMount的值不是"Default"
	//      3. runtime为docker
	//      4. docker的apiserver版本小于1.38.0 
	for _, handler := range kl.softAdmitHandlers {
		if result := handler.Admit(attrs); !result.Admit {
			return result
		}
	}

	return lifecycle.PodAdmitResult{Admit: true}
}

// syncLoop is the main loop for processing changes. It watches for changes from
// three channels (file, apiserver, and http) and creates a union of them. For
// any new change seen, will run a sync against desired state and running state. If
// no changes are seen to the configuration, will synchronize the last known desired
// state every sync-frequency seconds. Never returns.
func (kl *Kubelet) syncLoop(updates <-chan kubetypes.PodUpdate, handler SyncHandler) {
	klog.Info("Starting kubelet main sync loop.")
	// The syncTicker wakes up kubelet to checks if there are any pod workers
	// that need to be sync'd. A one-second period is sufficient because the
	// sync interval is defaulted to 10s.
	syncTicker := time.NewTicker(time.Second)
	defer syncTicker.Stop()
	housekeepingTicker := time.NewTicker(housekeepingPeriod)
	defer housekeepingTicker.Stop()
	// 返回kl.pleg.eventChannel（接收PodLifecycleEvent）
	plegCh := kl.pleg.Watch()
	const (
		base   = 100 * time.Millisecond
		max    = 5 * time.Second
		factor = 2
	)
	duration := base
	// Responsible for checking limits in resolv.conf
	// The limits do not have anything to do with individual pods
	// Since this is called in syncLoop, we don't need to call it anywhere else
	// kl.dnsConfigurer默认不为空，kl.dnsConfigurer.ResolverConfig默认为"/etc/resolv.conf"
	if kl.dnsConfigurer != nil && kl.dnsConfigurer.ResolverConfig != "" {
		// 检查宿主机的/etc/resolv.conf的search domain个数是否超出限制
		// 1. c.ClusterDomain不为空，则search domain个数最多为3个
		// 2. c.ClusterDomain为空，则search domain个数最多为6个
		// 检查主机的/etc/resolv.conf的search domain长度不能超过256个字符
		kl.dnsConfigurer.CheckLimitsForResolvConf()
	}

	for {
		// 如果runtime有错误，则进行指数回退，再次判断runtime是否有错误，直到runtime没有错误
		if err := kl.runtimeState.runtimeErrors(); err != nil {
			klog.Errorf("skipping pod synchronization - %v", err)
			// exponential backoff
			time.Sleep(duration)
			duration = time.Duration(math.Min(float64(max), factor*float64(duration)))
			continue
		}
		// reset backoff if we have a success
		// runtime没有错误，则重置周期时间为初始时间周期
		duration = base

		// 保存syncloop操作的时间
		kl.syncLoopMonitor.Store(kl.clock.Now())
		// 这里的handler是kubelet
		if !kl.syncLoopIteration(updates, handler, syncTicker.C, housekeepingTicker.C, plegCh) {
			break
		}
		kl.syncLoopMonitor.Store(kl.clock.Now())
	}
}

// syncLoopIteration reads from various channels and dispatches pods to the
// given handler.
//
// Arguments:
// 1.  configCh:       a channel to read config events from
// 2.  handler:        the SyncHandler to dispatch pods to
// 3.  syncCh:         a channel to read periodic sync events from
// 4.  housekeepingCh: a channel to read housekeeping events from
// 5.  plegCh:         a channel to read PLEG updates from
//
// Events are also read from the kubelet liveness manager's update channel.
//
// The workflow is to read from one of the channels, handle that event, and
// update the timestamp in the sync loop monitor.
//
// Here is an appropriate place to note that despite the syntactical
// similarity to the switch statement, the case statements in a select are
// evaluated in a pseudorandom order if there are multiple channels ready to
// read from when the select is evaluated.  In other words, case statements
// are evaluated in random order, and you can not assume that the case
// statements evaluate in order if multiple channels have events.
//
// With that in mind, in truly no particular order, the different channels
// are handled as follows:
//
// * configCh: dispatch the pods for the config change to the appropriate
//             handler callback for the event type
// * plegCh: update the runtime cache; sync pod
// * syncCh: sync all pods waiting for sync
// * housekeepingCh: trigger cleanup of pods
// * liveness manager: sync pods that have failed or in which one or more
//                     containers have failed liveness checks
func (kl *Kubelet) syncLoopIteration(configCh <-chan kubetypes.PodUpdate, handler SyncHandler,
	syncCh <-chan time.Time, housekeepingCh <-chan time.Time, plegCh <-chan *pleg.PodLifecycleEvent) bool {
	select {
	case u, open := <-configCh:
		// Update from a config source; dispatch it to the right handler
		// callback.
		if !open {
			klog.Errorf("Update channel is closed. Exiting the sync loop.")
			return false
		}

		switch u.Op {
		case kubetypes.ADD:
			klog.V(2).Infof("SyncLoop (ADD, %q): %q", u.Source, format.Pods(u.Pods))
			// After restarting, kubelet will get all existing pods through
			// ADD as if they are new pods. These pods will then go through the
			// admission process and *may* be rejected. This can be resolved
			// once we have checkpointing.
			// 让kubelet更新secret和configmap的reflector，将pod添加到kl.podManage
			// 如果设置了bootstrapCheckpointPath，则将pod信息保存到pod的checkpoint文件中
			// 如果pod是mirror pod
			// 让podworker启动一个goroutine，调用kl.syncPod来
			//   创建mirror pod，更新pod.Status状态，创建podsandbox和有init container则启动第一个init container，没有init container，则启动所有container
			//   删除mirror pod，并创建一个新的mirror pod
			//   更新mirror pod status
			// pod的Phase不为"Failed"和"Succeeded"，且（pod未被删除或pod被删除且不是所有container都处于Terminated或Waiting）
			//   进行 pod admit，包括evictionAdmitHandler、sysctlsWhitelist、AllocateResourcesPodAdmitHandler admit、PredicateAdmitHandler
			// admit通过后，利用podworker机制进行启动pod
			// pod里所有有定义的probe的container，每个container里probe配置，启动一个goroutine进行probe，probe结果发生变化，则更新pod status
			handler.HandlePodAdditions(u.Pods)
		case kubetypes.UPDATE:
			// format.PodsWithDeletionTimestamps(u.Pods)
			// pod被删除，输出"{podName}_{podNamespace}({podUID}):DeletionTimestamp=2006-01-02T15:04:05Z07:00"
			// 否则输出"{podName}_{podNamespace}({podUID})"
			klog.V(2).Infof("SyncLoop (UPDATE, %q): %q", u.Source, format.PodsWithDeletionTimestamps(u.Pods))
			handler.HandlePodUpdates(u.Pods)
		case kubetypes.REMOVE:
			klog.V(2).Infof("SyncLoop (REMOVE, %q): %q", u.Source, format.Pods(u.Pods))
			// 在kl.podManager.secretManager.manager.objectCache减少pod相关secret或configmap的引用计数，相关的secret或configmap次数为0，则停止这secret或configmap的reflector
			// 在kl.podManager.configMapManager.manager.objectCache减少pod相关secret或configmap的引用计数，相关的secret或configmap次数为0，则停止这secret或configmap的reflector
			// 如果是mirror pod，则从kl.podManager.mirrorPodByUID、kl.podManager.mirrorPodByFullName、kl.podManager.translationByUID删除对应的pod
			// 不是mirror pod，则从kl.podManager.podByUID、kl.podManager.podByFullName中删除这个pod
			// 如果启用pod checkpoint manager，则删除pod的checkpoint文件
			// 如果是mirror pod， 让podworker启动一个goroutine，调用kl.syncPod来
			//   创建mirror pod，更新pod.Status状态，创建podsandbox和有init container则启动第一个init container，没有init container，则启动所有container
			//   删除mirror pod，并创建一个新的mirror pod
			//   更新mirror pod status
			// 普通pod
			// kl.sourcesReady.seenSources里的所有源已经注册且都提供了至少一个pod
			// 如果pod在kl.podWorkers.podUpdates有UpdatePodOptions chan，则关闭这个chan，并从kl.podWorkers.podUpdates中移除。在kl.podWorkers.lastUndeliveredWorkUpdate里有pod未处理的事件，直接删除
			// 发送podPair消息给kl.podKillingCh，让kl.podKiller消费，进行pod删除（停止所有container）
			// 停止pod所有container（有定义probe）下的probe的worker
			handler.HandlePodRemoves(u.Pods)
		case kubetypes.RECONCILE:
			klog.V(4).Infof("SyncLoop (RECONCILE, %q): %q", u.Source, format.Pods(u.Pods))
			// 处理pod status更新
			handler.HandlePodReconcile(u.Pods)
		case kubetypes.DELETE:
			// REMOVE和DELETE区别：REMOVE是pod资源已经不存在了，而DELETE是pod的DeletionTimestamp不为nil（pod资源是存在的）
			klog.V(2).Infof("SyncLoop (DELETE, %q): %q", u.Source, format.Pods(u.Pods))
			// DELETE is treated as a UPDATE because of graceful deletion.
			// 只进行停止pod里所有container和sandbox，其他操作（比如从kl.podManager、处理mirror pod、从kl.probeManager和kl.podWorkers移除）在DELETE中操作
			handler.HandlePodUpdates(u.Pods)
		case kubetypes.RESTORE:
			klog.V(2).Infof("SyncLoop (RESTORE, %q): %q", u.Source, format.Pods(u.Pods))
			// These are pods restored from the checkpoint. Treat them as new
			// pods.
			// 从checkpoints restore则认为所有的pod都是新添加的pod
			handler.HandlePodAdditions(u.Pods)
		case kubetypes.SET:
			// TODO: Do we want to support this?
			klog.Errorf("Kubelet does not support snapshot update")
		}

		if u.Op != kubetypes.RESTORE {
			// If the update type is RESTORE, it means that the update is from
			// the pod checkpoints and may be incomplete. Do not mark the
			// source as ready.

			// Mark the source ready after receiving at least one update from the
			// source. Once all the sources are marked ready, various cleanup
			// routines will start reclaiming resources. It is important that this
			// takes place only after kubelet calls the update handler to process
			// the update to ensure the internal pod cache is up-to-date.
			// 将u.Source添加到kl.sourcesReady.sourcesSeen
			kl.sourcesReady.AddSource(u.Source)
		}
	case e := <-plegCh:
		// 这个event类型，"ContainerStarted"、"ContainerDied"、"ContainerRemoved"的PodLifecycleEvent
		// event类型不是"ContainerRemoved"，则返回true
		if isSyncPodWorthy(e) {
			// PLEG event for a pod; sync it.
			// 根据uid返回非mirror pod（普通pod和static pod）
			if pod, ok := kl.podManager.GetPodByUID(e.ID); ok {
				klog.V(2).Infof("SyncLoop (PLEG): %q, event: %#v", format.Pod(pod), e)
				// 让pod的pod worker处理这些pod，执行kl.syncPod
				handler.HandlePodSyncs([]*v1.Pod{pod})
			} else {
				// If the pod no longer exists, ignore the event.
				// pod不存在，只记录这个event
				klog.V(4).Infof("SyncLoop (PLEG): ignore irrelevant event: %#v", e)
			}
		}

		// event类型是"ContainerDied"
		if e.Type == pleg.ContainerDied {
			if containerID, ok := e.Data.(string); ok {
				// pod在runtime中，也在apiserver中（或kubelet中static pod），且pod被evicted，或pod被删除且所有container的state都处于Terminated或Waiting，则removeAll为true
				// 从runtime中获得podStatus
				// 找到podStatus里所有exitedContainerID的container status里的status为"exited"的container status
				// 如果removeAll为true，则移除所有退出的container。否则保留最近kl.containerDeletor.containersToKeep个退出的容器
				// 发送所有需要移除的container给p.worker通道，让kl.containerDeletor里的goroutine消费这个通道进行移除这个container 
				kl.cleanUpContainersInPod(e.ID, containerID)
			}
		}
	case <-syncCh:
		// Sync pods waiting for sync
		// 返回的pod包含
		// 1. 这个pod uid是在pod worker中处理过的uid
		// 2. pod中还未被pod worker处理完，让kl.podSyncLoopHandler决定（返回true的）加入podsToSync返回列表里
		// kl.podSyncLoopHandler只有一个pkg\kubelet\active_deadline.go activeDeadlineHandler
		// activeDeadlineHandler
		// 检测pod status中StartTime距现在时间长度，超出pod.Spec.ActiveDeadlineSeconds，返回true
		// 即pod的active时间长度是否超过pod.Spec.ActiveDeadlineSeconds
		podsToSync := kl.getPodsToSync()
		if len(podsToSync) == 0 {
			break
		}
		klog.V(4).Infof("SyncLoop (SYNC): %d pods; %s", len(podsToSync), format.Pods(podsToSync))
		// 让所有podsToSync的pod worker处理这些pod，执行kl.syncPod
		handler.HandlePodSyncs(podsToSync)
	case update := <-kl.livenessManager.Updates():
		// liveness probe发生失败
		if update.Result == proberesults.Failure {
			// The liveness manager detected a failure; sync the pod.

			// We should not use the pod from livenessManager, because it is never updated after
			// initialization.
			pod, ok := kl.podManager.GetPodByUID(update.PodUID)
			if !ok {
				// If the pod no longer exists, ignore the update.
				klog.V(4).Infof("SyncLoop (container unhealthy): ignore irrelevant update: %#v", update)
				break
			}
			klog.V(1).Infof("SyncLoop (container unhealthy): %q", format.Pod(pod))
			// 让pod的pod worker处理pod，在kl.syncPod里kl.containerRuntime.SyncPod，会停止现有unhealthy container
			handler.HandlePodSyncs([]*v1.Pod{pod})
		}
	case <-housekeepingCh:
		// kl.sourcesReady.seenSources里的不是所有源已经注册且都提供了至少一个pod
		if !kl.sourcesReady.AllReady() {
			// If the sources aren't ready or volume manager has not yet synced the states,
			// skip housekeeping, as we may accidentally delete pods from unready sources.
			klog.V(4).Infof("SyncLoop (housekeeping, skipped): sources aren't ready yet.")
		} else {
			klog.V(4).Infof("SyncLoop (housekeeping)")
			// 停止不存在的pod的pod worker，并停止周期性执行container的readniess、liveness、start probe的worker
			// 移除不存在pod的容器（容器还在运行，但是不在apiserver中）
			// 移除不存在pod的volume和pod目录，当"/var/lib/kubelet/pods/{podUID}/volume"或volume-subpaths目录都不存在
			// 从apiserver中删除所有没有对应static pod的mirror pod
			// 启用cgroupsPerQOS，则处理cgroup路径下不存在的pod
			//   如果pod还存在volume，且不启用keepTerminatedPodVolumes，则pod的cpu cgroup里的cpu share的值为2
			//   否则启动一个goroutine 移除所有cgroup子系统里的cgroup路径
			// 容器重启的回退周期清理，清理开始回退时间离现在已经超过2倍最大回收周期的item
			if err := handler.HandlePodCleanups(); err != nil {
				klog.Errorf("Failed cleaning pods: %v", err)
			}
		}
	}
	return true
}

// dispatchWork starts the asynchronous sync of the pod in a pod worker.
// If the pod has completed termination, dispatchWork will perform no action.
// 根据pod.Status状态，判断需要执行的动作
// 如果pod被删除且pod里的所有container的state都处于Terminated或Waiting，则更新pod status，并在apiserver中再次删除pod
// 如果pod的Phase不为"Failed"和"Succeeded"，且pod未被删除或pod被删除且不是所有container都处于Terminated和Waiting状态，则让podworke里启动goroutine的r进行处理
// 确保每个pod uid有一个goroutine来处理UpdatePodOptions
// 根据goroutine的工作状态，来处理UpdatePodOptions。如果goroutine不在工作中，则UpdatePodOptions投递到chan中（让goroutine处理，最终会调用kl.syncPod）。如果goroutine在工作中则，将UpdatePodOption放到暂存器中（等待goroutine处理完当前消息后，再来处理）。
func (kl *Kubelet) dispatchWork(pod *v1.Pod, syncType kubetypes.SyncPodType, mirrorPod *v1.Pod, start time.Time) {
	// check whether we are ready to delete the pod from the API server (all status up to date)
	// 是否 所有container的state都处于Terminated或Waiting
	// 是否 pod的Phase为"Failed"或"Succeeded"，或pod被删除且所有container都处于Terminated或Waiting
	containersTerminal, podWorkerTerminal := kl.podAndContainersAreTerminal(pod)
	// pod处于被删除状态且所有container的state都处于Terminated或Waiting，则执行最后在api server中删除pod（设置GracePeriodSeconds为0进行删除）
	if pod.DeletionTimestamp != nil && containersTerminal {
		klog.V(4).Infof("Pod %q has completed execution and should be deleted from the API server: %s", format.Pod(pod), syncType)
		// 设置所有非Terminated或非Waiting的container status为Terminated状态
		// 如果pod status发生变化，则更新kl.statusManager.podStatuses字段中的pod状态，并添加需要更新的status到kl.statusManager.podStatusChannel
		// 如果kl.statusManager.podStatusChannel满了，则直接放弃，等待周期性kl.statusManager.syncBatch进行同步
		// 
		// 在kl.statusManager.Start()会启动一个go routine来消费kl.statusManager.podStatusChannel，更新pod的status和还处理了pod已经可以被删除情况，在api server中删除pod（设置GracePeriodSeconds为0进行删除）。
		// 和每10s进行批量执行更新pod的status和处理pod已经可以被删除情况，在api server中删除pod（设置GracePeriodSeconds为0进行删除）
		kl.statusManager.TerminatePod(pod)
		return
	}

	// optimization: avoid invoking the pod worker if no further changes are possible to the pod definition
	// pod的Phase为"Failed"或"Succeeded"，或pod被删除且所有container都处于Terminated或Waiting，直接返回
	if podWorkerTerminal {
		klog.V(4).Infof("Pod %q has completed, ignoring remaining sync work: %s", format.Pod(pod), syncType)
		return
	}

	// Run the sync in an async worker.
	// 确保每个pod uid有一个goroutine来处理UpdatePodOptions
	// 根据goroutine的工作状态，来处理UpdatePodOptions。如果goroutine不在工作中，则UpdatePodOptions投递到chan中（让goroutine处理，最终会调用kl.syncPod）。如果goroutine在工作中则，将UpdatePodOption放到暂存器中（等待goroutine处理完当前消息后，再来处理）。
	kl.podWorkers.UpdatePod(&UpdatePodOptions{
		Pod:        pod,
		MirrorPod:  mirrorPod,
		// 如果handleMirrorPod调用dispatchWork，则UpdateType为kubetypes.SyncPodUpdate
		UpdateType: syncType,
		// metric里记录的podWorkers耗时
		OnCompleteFunc: func(err error) {
			if err != nil {
				metrics.PodWorkerDuration.WithLabelValues(syncType.String()).Observe(metrics.SinceInSeconds(start))
			}
		},
	})
	// Note the number of containers for new pods.
	if syncType == kubetypes.SyncPodCreate {
		metrics.ContainersPerPodCount.Observe(float64(len(pod.Spec.Containers)))
	}
}

// TODO: handle mirror pods in a separate component (issue #17251)
// 让podworker启动一个goroutine，调用kl.syncPod来
// 更新mirror pod status
// 如果mirror pod被删除，则删除mirror pod，并创建一个新的mirror pod
// 根据runtime种container和sandbox的状态，计算出需要采取的动作
// 执行相关的动作，Kill pod sandbox、Kill any containers、 Create sandbox、Create ephemeral containers、Create init containers、Create normal containers
func (kl *Kubelet) handleMirrorPod(mirrorPod *v1.Pod, start time.Time) {
	// Mirror pod ADD/UPDATE/DELETE operations are considered an UPDATE to the
	// corresponding static pod. Send update to the pod worker if the static
	// pod exists.
	// 根据mirror pod获取static pod
	if pod, ok := kl.podManager.GetPodByMirrorPod(mirrorPod); ok {
		// 让podworker启动一个goroutine，调用kl.syncPod来删除、创建mirror pod、更新mirror podstatus
		// 根据runtime种container和sandbox的状态，计算出需要采取的动作
		// 执行相关的动作，Kill pod sandbox、Kill any containers、 Create sandbox、Create ephemeral containers、Create init containers、Create normal containers
		kl.dispatchWork(pod, kubetypes.SyncPodUpdate, mirrorPod, start)
	}
}

// HandlePodAdditions is the callback in SyncHandler for pods being added from
// a config source.
// 让kubelet更新secret和configmap的reflector，将pod添加到kl.podManage
// 如果设置了bootstrapCheckpointPath，则将pod信息保存到pod的checkpoint文件中
// 如果pod是mirror pod
// 让podworker启动一个goroutine，调用kl.syncPod来
//   创建mirror pod，更新pod.Status状态，创建podsandbox和有init container则启动第一个init container，没有init container，则启动所有container
//   删除mirror pod，并创建一个新的mirror pod
//   更新mirror pod status
// pod的Phase不为"Failed"和"Succeeded"，且（pod未被删除或pod被删除且不是所有container都处于Terminated或Waiting）
//   进行 pod admit，包括evictionAdmitHandler、sysctlsWhitelist、AllocateResourcesPodAdmitHandler admit、PredicateAdmitHandler
// admit通过后，利用podworker机制进行启动pod
// pod里所有有定义的probe的container，每个container里probe配置，启动一个goroutine进行probe，probe结果发生变化，则更新pod status
func (kl *Kubelet) HandlePodAdditions(pods []*v1.Pod) {
	start := kl.clock.Now()
	// 跟据pod的创建时间进行排序
	sort.Sort(sliceutils.PodsByCreationTime(pods))
	for _, pod := range pods {
		// 获取所有普通pod和static pod
		existingPods := kl.podManager.GetPods()
		// Always add the pod to the pod manager. Kubelet relies on the pod
		// manager as the source of truth for the desired state. If a pod does
		// not exist in the pod manager, it means that it has been deleted in
		// the apiserver and no action (other than cleanup) is required.
		// 让kubelet更新secret和configmap的reflector，将pod添加到kl.podManager对应的字段
		// pod是普通pod，则添加到kl.podManager的podByUID、podByFullName、mirrorPodByFullName字段
		// pod是mirror pod，则添加到kl.podManager的mirrorPodByUID、mirrorPodByFullName、mirrorPodByFullName字段
		// 如果设置了bootstrapCheckpointPath，则将pod信息保存到pod的checkpoint文件中
		kl.podManager.AddPod(pod)

		if kubetypes.IsMirrorPod(pod) {
			// 让podworker启动一个goroutine，调用kl.syncPod来
			// 更新mirror pod status
			// 如果mirror pod被删除，则删除mirror pod，并创建一个新的mirror pod
			// 根据runtime种container和sandbox的状态，计算出需要采取的动作
			// 执行相关的动作，Kill pod sandbox、Kill any containers、 Create sandbox、Create ephemeral containers、Create init containers、Create normal containers
			kl.handleMirrorPod(pod, start)
			continue
		}

		// pod的Phase不为"Failed"和"Succeeded"，且（pod未被删除或pod被删除且不是所有container都处于Terminated或Waiting）
		if !kl.podIsTerminated(pod) {
			// Only go through the admission process if the pod is not
			// terminated.

			// We failed pods that we rejected, so activePods include all admitted
			// pods that are alive.
			// 返回所有pods中，pod的Phase不是"Failed"和"Succeeded"，或pod没有被删除或pod被删除且至少一个container为running
			activePods := kl.filterOutTerminatedPods(existingPods)

			// Check if we can admit the pod; if not, reject it.
			//  通过evictionAdmitHandler admit条件（下列条件满足一个）
			//
			// 1. 所有资源没有达到evict阈值，则直接通过admit
			// 2. pod是static pod或mirror pod，或pod设置了优先级且优先级大于2000000000，则直接通过admit
			// 3. 只有一个"MemoryPressure" condition，qos不为"BestEffort"的pod，则直接通过admit
			// 4. 只有一个"MemoryPressure" condition，如果"BestEffort"的pod能够容忍taint "node.kubernetes.io/memory-pressure"、effect为"NoSchedule"，则直接通过admit

			// 通过sysctlsWhitelist admit条件
			// 不是这三种情况
			// 1. sysctl的namespace group为"ipc"且设置了hostIPC共享
			// 2. sysctl的namespace group为"net"且设置了hostNet共享
			// 3. sysctl不在白名单里
			//
			// 通过AllocateResourcesPodAdmitHandler admit条件
			// 实现为containerManagerImpl.resourceAllocator
			// 1. 成功为pod里所有普通container和init container，分配container的limit中所有需要device plugins的资源，且成功分配cpu（如果policy为static policy，成功为Guaranteed的pod且container request的cpu为整数的container，分配cpu）
			// 实现为containerManagerImpl.topologyManager
			// 1. policy为"none"，行为跟pkg\kubelet\cm\container_manager_linux.go里的resourceAllocator一样
			//   成功为pod里所有普通container和init container，分配container的limit中所有需要device plugins的资源，且成功分配cpu（如果policy为static policy，成功为Guaranteed的pod且container request的cpu为整数的container，分配cpu），则admit通过
			// 2. policy为"best-effort"、"restricted"、"single-numa-node"
			//   遍历所有的hintProviders（cpu manager和device manager）生成container的各个资源的TopologyHint列表
			//   从各个资源的TopologyHint列表中挑出一个TopologyHint，进行组合成[]TopologyHint，然后跟default Affinity（亲和所有numaNode）进行与计算。
			//   在所有组合中，根据各个policy类型，挑选出最合适的TopologyHint。
			//   admit结果：
			//   policy为"best-effort"，则admit通过
			//   policy为"restricted"、"single-numa-node"，则为最合适TopologyHint的Preferred的值
			//
			// 通过lifecycle.NewPredicateAdmitHandler admit条件（且的关系）
			//
			// 1. node可以分配的资源可以满足，pod的request资源里的cpu、memory、EphemeralStorage、ScalarResources
			// 2. node匹配pod的Spec.NodeSelector和pod.Spec.Affinity
			// 3. pod.Spec.NodeName等于Node Name
			// 4. 检查pod里的host port与node上已经存在的host port不冲突
			//
			// 5. 只有node资源不满足，且pod是static pod、或mirror pod，或pod优先级大于等于2000000000
			//   根据最优抢占算法和pod qos筛选出被抢占的pod，这些被抢占pod的资源，能够满足admit pod需要
			if ok, reason, message := kl.canAdmitPod(activePods, pod); !ok {
				// 发送pod admit失败 event
				// 更新pod在status manager的status
				kl.rejectPod(pod, reason, message)
				continue
			}
		}
		mirrorPod, _ := kl.podManager.GetMirrorPodByPod(pod)
		// 利用podworker机制进行启动pod，pod被reject（不会启动）
		kl.dispatchWork(pod, kubetypes.SyncPodCreate, mirrorPod, start)
		// pod里所有有定义的probe的container，每个container添加一个worker到m.workers，
		// 并启动一个goroutine，周期性执行probe
		// 执行结果有变化，则写入worker.resultsManager.cache中，则发送Update消息到worker.resultsManager.updates通道
		// worker.resultsManager，是对应类型probe的manager，即kl.probeManage.readinessManager、kl.probeManage.livenessManager、kl.probeManage.startupManager
		kl.probeManager.AddPod(pod)
	}
}

// HandlePodUpdates is the callback in the SyncHandler interface for pods
// being updated from a config source.
func (kl *Kubelet) HandlePodUpdates(pods []*v1.Pod) {
	start := kl.clock.Now()
	for _, pod := range pods {
		// 让kubelet更新secret和configmap的manager里引用计数（已经记录的pod，不更新）（决定启动reflector或停止reflector，将pod添加到pm中对应的字段
		// pod是普通pod，则添加到podByUID、podByFullName、mirrorPodByFullName
		// pod是mirror pod，则添加到mirrorPodByUID、mirrorPodByFullName、mirrorPodByFullName
		// 如果设置了bootstrapCheckpointPath，则将pod信息保存到pod的checkpoint文件中
		kl.podManager.UpdatePod(pod)
		// 如果pod是mirror pod
		if kubetypes.IsMirrorPod(pod) {
			// 让podworker启动一个goroutine，调用kl.syncPod来
			// 更新mirror pod status
			// 如果mirror pod被删除，则删除mirror pod，并创建一个新的mirror pod
			// 根据runtime种container和sandbox的状态，计算出需要采取的动作
			// 执行相关的动作，Kill pod sandbox、Kill any containers、 Create sandbox、Create ephemeral containers、Create init containers、Create normal containers
			kl.handleMirrorPod(pod, start)
			continue
		}
		// TODO: Evaluate if we need to validate and reject updates.

		mirrorPod, _ := kl.podManager.GetMirrorPodByPod(pod)
		// 利用pod worker机制，执行更新pod状态更新（流程跟pod add一样，只是没有记录启动处理时间metrics）
		kl.dispatchWork(pod, kubetypes.SyncPodUpdate, mirrorPod, start)
	}
}

// HandlePodRemoves is the callback in the SyncHandler interface for pods
// being removed from a config source.
// 在kl.podManager.secretManager.manager.objectCache减少pod相关secret或configmap的引用计数，相关的secret或configmap次数为0，则停止这secret或configmap的reflector
// 在kl.podManager.configMapManager.manager.objectCache减少pod相关secret或configmap的引用计数，相关的secret或configmap次数为0，则停止这secret或configmap的reflector
// 如果是mirror pod，则从kl.podManager.mirrorPodByUID、kl.podManager.mirrorPodByFullName、kl.podManager.translationByUID删除对应的pod
// 不是mirror pod，则从kl.podManager.podByUID、kl.podManager.podByFullName中删除这个pod
// 如果启用pod checkpoint manager，则删除pod的checkpoint文件
// 如果是mirror pod， 让podworker启动一个goroutine，调用kl.syncPod来
//   创建mirror pod，更新pod.Status状态，创建podsandbox和有init container则启动第一个init container，没有init container，则启动所有container
//   删除mirror pod，并创建一个新的mirror pod
//   更新mirror pod status
// 普通pod
// kl.sourcesReady.seenSources里的所有源已经注册且都提供了至少一个pod
// 如果pod在kl.podWorkers.podUpdates有UpdatePodOptions chan，则关闭这个chan，并从kl.podWorkers.podUpdates中移除。在kl.podWorkers.lastUndeliveredWorkUpdate里有pod未处理的事件，直接删除
// 发送podPair消息给kl.podKillingCh，让kl.podKiller消费，进行pod删除（停止所有container）
// 停止pod所有container（有定义probe）下的probe的worker
// 这里重复的执行kl.killPod（停止所有container），pod被删除会触发kubetypes.DELETE时间--在kl.HandlePodUpdates里执行了kl.killPod
func (kl *Kubelet) HandlePodRemoves(pods []*v1.Pod) {
	start := kl.clock.Now()
	for _, pod := range pods {
		// 在kl.podManager.secretManager.manager.objectCache减少pod相关secret或configmap的引用计数，相关的secret或configmap次数为0，则停止这secret或configmap的reflector
		// 在kl.podManager.configMapManager.manager.objectCache减少pod相关secret或configmap的引用计数，相关的secret或configmap次数为0，则停止这secret或configmap的reflector
		// 如果是mirror pod，则从kl.podManager.mirrorPodByUID、kl.podManager.mirrorPodByFullName、kl.podManager.translationByUID删除对应的pod
		// 不是mirror pod，则从kl.podManager.podByUID、kl.podManager.podByFullName中删除这个pod
		// 如果启用pod checkpoint manager，则删除pod的checkpoint文件
		kl.podManager.DeletePod(pod)
		if kubetypes.IsMirrorPod(pod) {
			// 让podworker启动一个goroutine，调用kl.syncPod来
			// 更新mirror pod status
			// 如果mirror pod被删除，则删除mirror pod，并创建一个新的mirror pod
			// 根据runtime种container和sandbox的状态，计算出需要采取的动作
			// 执行相关的动作，Kill pod sandbox、Kill any containers、 Create sandbox、Create ephemeral containers、Create init containers、Create normal containers
			kl.handleMirrorPod(pod, start)
			continue
		}
		// Deletion is allowed to fail because the periodic cleanup routine
		// will trigger deletion again.
		// kl.sourcesReady.seenSources里的所有源已经注册且都提供了至少一个pod
		// 如果pod在kl.podWorkers.podUpdates有UpdatePodOptions chan，则关闭这个chan，并从kl.podWorkers.podUpdates中移除。在kl.podWorkers.lastUndeliveredWorkUpdate里有pod未处理的事件，直接删除
		// 发送podPair消息给kl.podKillingCh，让kl.podKiller消费，进行pod删除（停止所有container）
		// 这里重复的执行kl.killPod（停止所有container），pod被删除会触发kubetypes.DELETE时间--在kl.HandlePodUpdates里执行了kl.killPod
		if err := kl.deletePod(pod); err != nil {
			klog.V(2).Infof("Failed to delete pod %q, err: %v", format.Pod(pod), err)
		}
		// 停止pod所有container（有定义probe）下的probe的worker
		kl.probeManager.RemovePod(pod)
	}
}

// HandlePodReconcile is the callback in the SyncHandler interface for pods
// that should be reconciled.
// 让kubelet更新secret和configmap的的manager里引用计数（已经记录的pod，不更新）（决定启动reflector或停止reflector），将pod添加到pm中对应的字段
// pod是普通pod，则添加到podByUID、podByFullName、mirrorPodByFullName
// pod是mirror pod，则添加到mirrorPodByUID、mirrorPodByFullName、mirrorPodByFullName
// 如果设置了bootstrapCheckpointPath，则将pod信息保存到pod的checkpoint文件中
// 现在pod里有type为"Ready"的condition。且status字段跟要更新的（type为"Ready"的condition）不一样或Message字段不一样
//   则利用pod worker机制，执行更新pod状态更新（流程跟pod add一样，只是没有记录启动处理时间metrics），主要是为了更新pod status
// pod的Phase为"Failed"且reason为"Evicted"
//   获得缓存（kl.podCache.pods）中的id的data里status，如果不在缓存中，则返回空的数据（只有pod id）
//   找到podStatus里所有container status里的status为"exited"的container status
//   如果有filterContainerID过滤的container，则还要匹配过滤的container
//   返回保留最近containersToKeep个后剩余的container status（按照创建时间倒序排列）
//   发送所有container给kl.containerDeletor.worker通道，让goroutine消费这个通道进行移除这个container
func (kl *Kubelet) HandlePodReconcile(pods []*v1.Pod) {
	start := kl.clock.Now()
	for _, pod := range pods {
		// Update the pod in pod manager, status manager will do periodically reconcile according
		// to the pod manager.
		// 这里只更新pod manager，因为status manager会周期性的从pod manager中同步
		// 让kubelet更新secret和configmap的的manager里引用计数（已经记录的pod，不更新）（决定启动reflector或停止reflector），将pod添加到pm中对应的字段
		// pod是普通pod，则添加到podByUID、podByFullName、mirrorPodByFullName
		// pod是mirror pod，则添加到mirrorPodByUID、mirrorPodByFullName、mirrorPodByFullName
		// 如果设置了bootstrapCheckpointPath，则将pod信息保存到pod的checkpoint文件中
		kl.podManager.UpdatePod(pod)

		// Reconcile Pod "Ready" condition if necessary. Trigger sync pod for reconciliation.
		// 现在pod里有type为"Ready"的condition。且status字段跟生成的（type为"Ready"的condition）不一样或Message字段不一样，则返回true
		if status.NeedToReconcilePodReadiness(pod) {
			mirrorPod, _ := kl.podManager.GetMirrorPodByPod(pod)
			// 利用pod worker机制，执行更新pod状态更新（流程跟pod add一样，只是没有记录启动处理时间metrics），主要是为了更新pod status
			kl.dispatchWork(pod, kubetypes.SyncPodSync, mirrorPod, start)
		}

		// After an evicted pod is synced, all dead containers in the pod can be removed.
		// pod的Phase为"Failed"且reason为"Evicted"，返回true
		if eviction.PodIsEvicted(pod.Status) {
			// 获得缓存（kl.podCache.pods）中的id的data里status，如果不在缓存中，则返回空的数据（只有pod id）
			if podStatus, err := kl.podCache.Get(pod.UID); err == nil {
				// 找到podStatus里所有container status里的status为"exited"的container status
				// 如果有filterContainerID过滤的container，则还要匹配过滤的container
				// 返回保留最近containersToKeep个后剩余的container status（按照创建时间倒序排列）
				// 发送所有container给kl.containerDeletor.worker通道，让goroutine消费这个通道进行移除这个container
				kl.containerDeletor.deleteContainersInPod("", podStatus, true)
			}
		}
	}
}

// HandlePodSyncs is the callback in the syncHandler interface for pods
// that should be dispatched to pod workers for sync.
// 让所有pods的pod worker处理这些pods，执行kl.syncPod
func (kl *Kubelet) HandlePodSyncs(pods []*v1.Pod) {
	start := kl.clock.Now()
	for _, pod := range pods {
		mirrorPod, _ := kl.podManager.GetMirrorPodByPod(pod)
		kl.dispatchWork(pod, kubetypes.SyncPodSync, mirrorPod, start)
	}
}

// LatestLoopEntryTime returns the last time in the sync loop monitor.
func (kl *Kubelet) LatestLoopEntryTime() time.Time {
	val := kl.syncLoopMonitor.Load()
	if val == nil {
		return time.Time{}
	}
	return val.(time.Time)
}

// updateRuntimeUp calls the container runtime status callback, initializing
// the runtime dependent modules when the container runtime first comes up,
// and returns an error if the status check fails.  If the status check is OK,
// update the container runtime uptime in the kubelet runtimeState.
// 去访问docker获取cni的配置正确和docker是否存活等状态，启动依赖的模块（cadvisor、containerManager、evictionManager、containerLogManager、pluginManager）
func (kl *Kubelet) updateRuntimeUp() {
	kl.updateRuntimeMux.Lock()
	defer kl.updateRuntimeMux.Unlock()

	// 调用cri接口的Status查询 runtime status--包括RuntimeReady、NetworkReady
	// 如果container runtime是dockershim(在pkg\kubelet\dockershim\docker_service.go里(*dockerService).Status)
	// RuntimeReady是调用dockerService.client.Version()获取docker版本是否有问题
	// NetworkReady是调用dockerService.network.Status()获取网络插件状态是否有问题
	s, err := kl.containerRuntime.Status()
	if err != nil {
		klog.Errorf("Container runtime sanity check failed: %v", err)
		return
	}
	if s == nil {
		klog.Errorf("Container runtime status is nil")
		return
	}
	// Periodically log the whole runtime status for debugging.
	// TODO(random-liu): Consider to send node event when optional
	// condition is unmet.
	klog.V(4).Infof("Container runtime status: %v", s)
	networkReady := s.GetRuntimeCondition(kubecontainer.NetworkReady)
	if networkReady == nil || !networkReady.Status {
		klog.Errorf("Container runtime network not ready: %v", networkReady)
		kl.runtimeState.setNetworkState(fmt.Errorf("runtime network not ready: %v", networkReady))
	} else {
		// Set nil if the container runtime network is ready.
		kl.runtimeState.setNetworkState(nil)
	}
	// information in RuntimeReady condition will be propagated to NodeReady condition.
	runtimeReady := s.GetRuntimeCondition(kubecontainer.RuntimeReady)
	// If RuntimeReady is not set or is false, report an error.
	if runtimeReady == nil || !runtimeReady.Status {
		err := fmt.Errorf("Container runtime not ready: %v", runtimeReady)
		klog.Error(err)
		kl.runtimeState.setRuntimeState(err)
		return
	}
	kl.runtimeState.setRuntimeState(nil)
	kl.oneTimeInitializer.Do(kl.initializeRuntimeDependentModules)
	kl.runtimeState.setRuntimeSync(kl.clock.Now())
}

// GetConfiguration returns the KubeletConfiguration used to configure the kubelet.
func (kl *Kubelet) GetConfiguration() kubeletconfiginternal.KubeletConfiguration {
	return kl.kubeletConfiguration
}

// BirthCry sends an event that the kubelet has started up.
func (kl *Kubelet) BirthCry() {
	// Make an event that kubelet restarted.
	kl.recorder.Eventf(kl.nodeRef, v1.EventTypeNormal, events.StartingKubelet, "Starting kubelet.")
}

// ResyncInterval returns the interval used for periodic syncs.
func (kl *Kubelet) ResyncInterval() time.Duration {
	return kl.resyncInterval
}

// ListenAndServe runs the kubelet HTTP server.
func (kl *Kubelet) ListenAndServe(address net.IP, port uint, tlsOptions *server.TLSOptions, auth server.AuthInterface, enableCAdvisorJSONEndpoints, enableDebuggingHandlers, enableContentionProfiling bool) {
	server.ListenAndServeKubeletServer(kl, kl.resourceAnalyzer, address, port, tlsOptions, auth, enableCAdvisorJSONEndpoints, enableDebuggingHandlers, enableContentionProfiling, kl.redirectContainerStreaming, kl.criHandler)
}

// ListenAndServeReadOnly runs the kubelet HTTP server in read-only mode.
func (kl *Kubelet) ListenAndServeReadOnly(address net.IP, port uint, enableCAdvisorJSONEndpoints bool) {
	server.ListenAndServeKubeletReadOnlyServer(kl, kl.resourceAnalyzer, address, port, enableCAdvisorJSONEndpoints)
}

// ListenAndServePodResources runs the kubelet podresources grpc service
func (kl *Kubelet) ListenAndServePodResources() {
	socket, err := util.LocalEndpoint(kl.getPodResourcesDir(), podresources.Socket)
	if err != nil {
		klog.V(2).Infof("Failed to get local endpoint for PodResources endpoint: %v", err)
		return
	}
	server.ListenAndServePodResources(socket, kl.podManager, kl.containerManager)
}

// Delete the eligible dead container instances in a pod. Depending on the configuration, the latest dead containers may be kept around.
// pod在runtime中，也在apiserver中（或kubelet中static pod），且pod被evicted，或pod被删除且所有container的state都处于Terminated或Waiting，则removeAll为true
// 找到podStatus里所有exitedContainerID的container status里的status为"exited"的container status
// 如果removeAll为true，则移除所有退出的container。否则保留最近kl.containerDeletor.containersToKeep个退出的容器
// 发送所有需要移除的container给p.worker通道，让kl.containerDeletor里的goroutine消费这个通道进行移除这个container 
func (kl *Kubelet) cleanUpContainersInPod(podID types.UID, exitedContainerID string) {
	// 返回缓存（kl.podCache.pods）中的id的data里status，如果不在缓存中，则返回空的数据（只有pod id）
	if podStatus, err := kl.podCache.Get(podID); err == nil {
		removeAll := false
		// 根据uid返回非mirror pod（普通pod和static pod）
		if syncedPod, ok := kl.podManager.GetPodByUID(podID); ok {
			// pod在runtime中，也在apiserver中（或kubelet中static pod）

			// generate the api status using the cached runtime status to get up-to-date ContainerStatuses
			// 根据runtime的status生成pod status
			apiPodStatus := kl.generateAPIPodStatus(syncedPod, podStatus)
			// When an evicted or deleted pod has already synced, all containers can be removed.
			// pod被evicted，或pod被删除且所有container的state都处于Terminated或Waiting，则removeAll为true
			removeAll = eviction.PodIsEvicted(syncedPod.Status) || (syncedPod.DeletionTimestamp != nil && notRunning(apiPodStatus.ContainerStatuses))
		}
		// 找到podStatus里所有exitedContainerID的container status里的status为"exited"的container status
		// 如果removeAll为true，则移除所有退出的container。否则保留最近kl.containerDeletor.containersToKeep个退出的容器
		// 发送所有需要移除的container给kl.containerDeletor.worker通道，让goroutine消费这个通道进行移除这个container
		kl.containerDeletor.deleteContainersInPod(exitedContainerID, podStatus, removeAll)
	}
}

// fastStatusUpdateOnce starts a loop that checks the internal node indexer cache for when a CIDR
// is applied  and tries to update pod CIDR immediately. After pod CIDR is updated it fires off
// a runtime update and a node status update. Function returns after one successful node status update.
// Function is executed only during Kubelet start which improves latency to ready node by updating
// pod CIDR, runtime status and node statuses ASAP.
func (kl *Kubelet) fastStatusUpdateOnce() {
	for {
		time.Sleep(100 * time.Millisecond)
		node, err := kl.GetNode()
		if err != nil {
			klog.Errorf(err.Error())
			continue
		}
		if len(node.Spec.PodCIDRs) != 0 {
			podCIDRs := strings.Join(node.Spec.PodCIDRs, ",")
			if _, err := kl.updatePodCIDR(podCIDRs); err != nil {
				klog.Errorf("Pod CIDR update to %v failed %v", podCIDRs, err)
				continue
			}
			kl.updateRuntimeUp()
			kl.syncNodeStatus()
			return
		}
	}
}

// isSyncPodWorthy filters out events that are not worthy of pod syncing
// event类型不是"ContainerRemoved"，则返回true
func isSyncPodWorthy(event *pleg.PodLifecycleEvent) bool {
	// ContainerRemoved doesn't affect pod state
	return event.Type != pleg.ContainerRemoved
}

// Gets the streaming server configuration to use with in-process CRI shims.
func getStreamingConfig(kubeCfg *kubeletconfiginternal.KubeletConfiguration, kubeDeps *Dependencies, crOptions *config.ContainerRuntimeOptions) *streaming.Config {
	config := &streaming.Config{
		// 默认为4小时
		StreamIdleTimeout:               kubeCfg.StreamingConnectionIdleTimeout.Duration,
		// 固定为30s
		StreamCreationTimeout:           streaming.DefaultConfig.StreamCreationTimeout,
		// 固定为["channel.k8s.io", "v2.channel.k8s.io", "v3.channel.k8s.io", "v4.channel.k8s.io"]
		SupportedRemoteCommandProtocols: streaming.DefaultConfig.SupportedRemoteCommandProtocols,
		// 固定为["portforward.k8s.io"]
		SupportedPortForwardProtocols:   streaming.DefaultConfig.SupportedPortForwardProtocols,
	}
	// RedirectContainerStreaming默认为false
	if !crOptions.RedirectContainerStreaming {
		config.Addr = net.JoinHostPort("localhost", "0")
	} else {
		// Use a relative redirect (no scheme or host).
		config.BaseURL = &url.URL{
			Path: "/cri/",
		}
		if kubeDeps.TLSOptions != nil {
			config.TLSConfig = kubeDeps.TLSOptions.Config
		}
	}
	return config
}
