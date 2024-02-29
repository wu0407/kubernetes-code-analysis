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
	"os"
	"path"
	sysruntime "runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"k8s.io/client-go/informers"

	cadvisorapi "github.com/google/cadvisor/info/v1"
	libcontaineruserns "github.com/opencontainers/runc/libcontainer/userns"
	"k8s.io/mount-utils"
	"k8s.io/utils/integer"
	netutils "k8s.io/utils/net"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
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
	"k8s.io/component-helpers/apimachinery/lease"
	internalapi "k8s.io/cri-api/pkg/apis"
	"k8s.io/klog/v2"
	pluginwatcherapi "k8s.io/kubelet/pkg/apis/pluginregistration/v1"
	statsapi "k8s.io/kubelet/pkg/apis/stats/v1alpha1"
	"k8s.io/kubernetes/pkg/features"
	kubeletconfiginternal "k8s.io/kubernetes/pkg/kubelet/apis/config"
	"k8s.io/kubernetes/pkg/kubelet/apis/podresources"
	"k8s.io/kubernetes/pkg/kubelet/cadvisor"
	kubeletcertificate "k8s.io/kubernetes/pkg/kubelet/certificate"
	"k8s.io/kubernetes/pkg/kubelet/cloudresource"
	"k8s.io/kubernetes/pkg/kubelet/cm"
	"k8s.io/kubernetes/pkg/kubelet/config"
	"k8s.io/kubernetes/pkg/kubelet/configmap"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/cri/remote"
	"k8s.io/kubernetes/pkg/kubelet/cri/streaming"
	"k8s.io/kubernetes/pkg/kubelet/events"
	"k8s.io/kubernetes/pkg/kubelet/eviction"
	"k8s.io/kubernetes/pkg/kubelet/images"
	"k8s.io/kubernetes/pkg/kubelet/kubeletconfig"
	"k8s.io/kubernetes/pkg/kubelet/kuberuntime"
	"k8s.io/kubernetes/pkg/kubelet/legacy"
	"k8s.io/kubernetes/pkg/kubelet/lifecycle"
	"k8s.io/kubernetes/pkg/kubelet/logs"
	"k8s.io/kubernetes/pkg/kubelet/metrics"
	"k8s.io/kubernetes/pkg/kubelet/metrics/collectors"
	"k8s.io/kubernetes/pkg/kubelet/network/dns"
	"k8s.io/kubernetes/pkg/kubelet/nodeshutdown"
	oomwatcher "k8s.io/kubernetes/pkg/kubelet/oom"
	"k8s.io/kubernetes/pkg/kubelet/pleg"
	"k8s.io/kubernetes/pkg/kubelet/pluginmanager"
	plugincache "k8s.io/kubernetes/pkg/kubelet/pluginmanager/cache"
	kubepod "k8s.io/kubernetes/pkg/kubelet/pod"
	"k8s.io/kubernetes/pkg/kubelet/preemption"
	"k8s.io/kubernetes/pkg/kubelet/prober"
	proberesults "k8s.io/kubernetes/pkg/kubelet/prober/results"
	"k8s.io/kubernetes/pkg/kubelet/runtimeclass"
	"k8s.io/kubernetes/pkg/kubelet/secret"
	"k8s.io/kubernetes/pkg/kubelet/server"
	servermetrics "k8s.io/kubernetes/pkg/kubelet/server/metrics"
	serverstats "k8s.io/kubernetes/pkg/kubelet/server/stats"
	"k8s.io/kubernetes/pkg/kubelet/stats"
	"k8s.io/kubernetes/pkg/kubelet/status"
	"k8s.io/kubernetes/pkg/kubelet/sysctl"
	"k8s.io/kubernetes/pkg/kubelet/token"
	kubetypes "k8s.io/kubernetes/pkg/kubelet/types"
	"k8s.io/kubernetes/pkg/kubelet/util"
	"k8s.io/kubernetes/pkg/kubelet/util/manager"
	"k8s.io/kubernetes/pkg/kubelet/util/queue"
	"k8s.io/kubernetes/pkg/kubelet/util/sliceutils"
	"k8s.io/kubernetes/pkg/kubelet/volumemanager"
	"k8s.io/kubernetes/pkg/security/apparmor"
	sysctlallowlist "k8s.io/kubernetes/pkg/security/podsecuritypolicy/sysctl"
	"k8s.io/kubernetes/pkg/util/oom"
	"k8s.io/kubernetes/pkg/util/selinux"
	"k8s.io/kubernetes/pkg/volume"
	"k8s.io/kubernetes/pkg/volume/csi"
	"k8s.io/kubernetes/pkg/volume/util/hostutil"
	"k8s.io/kubernetes/pkg/volume/util/subpath"
	"k8s.io/kubernetes/pkg/volume/util/volumepathhandler"
	"k8s.io/utils/clock"
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

	// Period for performing global cleanup tasks.
	housekeepingPeriod = time.Second * 2

	// Duration at which housekeeping failed to satisfy the invariant that
	// housekeeping should be fast to avoid blocking pod config (while
	// housekeeping is running no new pods are started or deleted).
	housekeepingWarningDuration = time.Second * 15

	// Period for performing eviction monitoring.
	// ensure this is kept in sync with internal cadvisor housekeeping.
	evictionMonitoringPeriod = time.Second * 10

	// The path in containers' filesystems where the hosts file is mounted.
	linuxEtcHostsPath   = "/etc/hosts"
	windowsEtcHostsPath = "C:\\Windows\\System32\\drivers\\etc\\hosts"

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

	// nodeLeaseRenewIntervalFraction is the fraction of lease duration to renew the lease
	nodeLeaseRenewIntervalFraction = 0.25
)

var etcHostsPath = getContainerEtcHostsPath()

func getContainerEtcHostsPath() string {
	if sysruntime.GOOS == "windows" {
		return windowsEtcHostsPath
	}
	return linuxEtcHostsPath
}

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
	ListenAndServe(kubeCfg *kubeletconfiginternal.KubeletConfiguration, tlsOptions *server.TLSOptions, auth server.AuthInterface)
	ListenAndServeReadOnly(address net.IP, port uint)
	ListenAndServePodResources()
	Run(<-chan kubetypes.PodUpdate)
	RunOnce(<-chan kubetypes.PodUpdate) ([]RunPodResult, error)
}

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
	DockerOptions           *DockerOptions
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
	dockerLegacyService     legacy.DockerLegacyService
	// remove it after cadvisor.UsingLegacyCadvisorStats dropped.
	useLegacyCadvisorStats bool
}

// DockerOptions contains docker specific configuration. Importantly, since it
// lives outside of `dockershim`, it should not depend on the `docker/docker`
// client library.
type DockerOptions struct {
	DockerEndpoint            string
	RuntimeRequestTimeout     time.Duration
	ImagePullProgressDeadline time.Duration
}

// makePodSourceConfig creates a config.PodConfig from the given
// KubeletConfiguration or returns an error.
func makePodSourceConfig(kubeCfg *kubeletconfiginternal.KubeletConfiguration, kubeDeps *Dependencies, nodeName types.NodeName, nodeHasSynced func() bool) (*config.PodConfig, error) {
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
	if kubeCfg.StaticPodPath != "" {
		klog.InfoS("Adding static pod path", "path", kubeCfg.StaticPodPath)
		config.NewSourceFile(kubeCfg.StaticPodPath, nodeName, kubeCfg.FileCheckFrequency.Duration, cfg.Channel(kubetypes.FileSource))
	}

	// define url config source
	if kubeCfg.StaticPodURL != "" {
		klog.InfoS("Adding pod URL with HTTP header", "URL", kubeCfg.StaticPodURL, "header", manifestURLHeader)
		config.NewSourceURL(kubeCfg.StaticPodURL, manifestURLHeader, nodeName, kubeCfg.HTTPCheckFrequency.Duration, cfg.Channel(kubetypes.HTTPSource))
	}

	if kubeDeps.KubeClient != nil {
		klog.InfoS("Adding apiserver pod source")
		config.NewSourceApiserver(kubeDeps.KubeClient, nodeName, nodeHasSynced, cfg.Channel(kubetypes.ApiserverSource))
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
		if remoteImageEndpoint == "" {
			remoteImageEndpoint = remoteRuntimeEndpoint
		}
	}

	switch containerRuntime {
	case kubetypes.DockerContainerRuntime:
		klog.InfoS("Using dockershim is deprecated, please consider using a full-fledged CRI implementation")
		if err := runDockershim(
			kubeCfg,
			kubeDeps,
			crOptions,
			runtimeCgroups,
			remoteRuntimeEndpoint,
			remoteImageEndpoint,
			nonMasqueradeCIDR,
		); err != nil {
			return err
		}
	case kubetypes.RemoteContainerRuntime:
		// No-op.
		break
	default:
		return fmt.Errorf("unsupported CRI runtime: %q", containerRuntime)
	}

	var err error
	if kubeDeps.RemoteRuntimeService, err = remote.NewRemoteRuntimeService(remoteRuntimeEndpoint, kubeCfg.RuntimeRequestTimeout.Duration); err != nil {
		return err
	}
	if kubeDeps.RemoteImageService, err = remote.NewRemoteImageService(remoteImageEndpoint, kubeCfg.RuntimeRequestTimeout.Duration); err != nil {
		return err
	}

	kubeDeps.useLegacyCadvisorStats = cadvisor.UsingLegacyCadvisorStats(containerRuntime, remoteRuntimeEndpoint)

	return nil
}

// NewMainKubelet instantiates a new Kubelet object along with all the required internal modules.
// No initialization of Kubelet and its modules should happen here.
func NewMainKubelet(kubeCfg *kubeletconfiginternal.KubeletConfiguration,
	kubeDeps *Dependencies,
	crOptions *config.ContainerRuntimeOptions,
	containerRuntime string,
	hostname string,
	hostnameOverridden bool,
	nodeName types.NodeName,
	nodeIPs []net.IP,
	providerID string,
	cloudProvider string,
	certDirectory string,
	rootDirectory string,
	imageCredentialProviderConfigFile string,
	imageCredentialProviderBinDir string,
	registerNode bool,
	registerWithTaints []v1.Taint,
	allowedUnsafeSysctls []string,
	experimentalMounterPath string,
	kernelMemcgNotification bool,
	experimentalCheckNodeCapabilitiesBeforeMount bool,
	experimentalNodeAllocatableIgnoreEvictionThreshold bool,
	minimumGCAge metav1.Duration,
	maxPerPodContainerCount int32,
	maxContainerCount int32,
	masterServiceNamespace string,
	registerSchedulable bool,
	keepTerminatedPodVolumes bool,
	nodeLabels map[string]string,
	nodeStatusMaxImages int32,
	seccompDefault bool,
) (*Kubelet, error) {
	if rootDirectory == "" {
		return nil, fmt.Errorf("invalid root directory %q", rootDirectory)
	}
	if kubeCfg.SyncFrequency.Duration <= 0 {
		return nil, fmt.Errorf("invalid sync frequency %d", kubeCfg.SyncFrequency.Duration)
	}

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

	if utilfeature.DefaultFeatureGate.Enabled(features.DisableCloudProviders) && cloudprovider.IsDeprecatedInternal(cloudProvider) {
		cloudprovider.DisableWarningForProvider(cloudProvider)
		return nil, fmt.Errorf("cloud provider %q was specified, but built-in cloud providers are disabled. Please set --cloud-provider=external and migrate to an external cloud provider", cloudProvider)
	}

	var nodeHasSynced cache.InformerSynced
	var nodeLister corelisters.NodeLister

	// If kubeClient == nil, we are running in standalone mode (i.e. no API servers)
	// If not nil, we are running as part of a cluster and should sync w/API
	if kubeDeps.KubeClient != nil {
		kubeInformers := informers.NewSharedInformerFactoryWithOptions(kubeDeps.KubeClient, 0, informers.WithTweakListOptions(func(options *metav1.ListOptions) {
			options.FieldSelector = fields.Set{metav1.ObjectNameField: string(nodeName)}.String()
		}))
		nodeLister = kubeInformers.Core().V1().Nodes().Lister()
		nodeHasSynced = func() bool {
			return kubeInformers.Core().V1().Nodes().Informer().HasSynced()
		}
		kubeInformers.Start(wait.NeverStop)
		klog.InfoS("Attempting to sync node with API server")
	} else {
		// we don't have a client to sync!
		nodeIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
		nodeLister = corelisters.NewNodeLister(nodeIndexer)
		nodeHasSynced = func() bool { return true }
		klog.InfoS("Kubelet is running in standalone mode, will skip API server sync")
	}

	if kubeDeps.PodConfig == nil {
		var err error
		kubeDeps.PodConfig, err = makePodSourceConfig(kubeCfg, kubeDeps, nodeName, nodeHasSynced)
		if err != nil {
			return nil, err
		}
	}

	containerGCPolicy := kubecontainer.GCPolicy{
		MinAge:             minimumGCAge.Duration,
		MaxPerPodContainer: int(maxPerPodContainerCount),
		MaxContainers:      int(maxContainerCount),
	}

	daemonEndpoints := &v1.NodeDaemonEndpoints{
		KubeletEndpoint: v1.DaemonEndpoint{Port: kubeCfg.Port},
	}

	imageGCPolicy := images.ImageGCPolicy{
		MinAge:               kubeCfg.ImageMinimumGCAge.Duration,
		HighThresholdPercent: int(kubeCfg.ImageGCHighThresholdPercent),
		LowThresholdPercent:  int(kubeCfg.ImageGCLowThresholdPercent),
	}

	enforceNodeAllocatable := kubeCfg.EnforceNodeAllocatable
	if experimentalNodeAllocatableIgnoreEvictionThreshold {
		// Do not provide kubeCfg.EnforceNodeAllocatable to eviction threshold parsing if we are not enforcing Evictions
		enforceNodeAllocatable = []string{}
	}
	thresholds, err := eviction.ParseThresholdConfig(enforceNodeAllocatable, kubeCfg.EvictionHard, kubeCfg.EvictionSoft, kubeCfg.EvictionSoftGracePeriod, kubeCfg.EvictionMinimumReclaim)
	if err != nil {
		return nil, err
	}
	evictionConfig := eviction.Config{
		PressureTransitionPeriod: kubeCfg.EvictionPressureTransitionPeriod.Duration,
		MaxPodGracePeriodSeconds: int64(kubeCfg.EvictionMaxPodGracePeriod),
		Thresholds:               thresholds,
		KernelMemcgNotification:  kernelMemcgNotification,
		PodCgroupRoot:            kubeDeps.ContainerManager.GetPodCgroupRoot(),
	}

	var serviceLister corelisters.ServiceLister
	var serviceHasSynced cache.InformerSynced
	if kubeDeps.KubeClient != nil {
		kubeInformers := informers.NewSharedInformerFactory(kubeDeps.KubeClient, 0)
		serviceLister = kubeInformers.Core().V1().Services().Lister()
		serviceHasSynced = kubeInformers.Core().V1().Services().Informer().HasSynced
		kubeInformers.Start(wait.NeverStop)
	} else {
		serviceIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
		serviceLister = corelisters.NewServiceLister(serviceIndexer)
		serviceHasSynced = func() bool { return true }
	}

	// construct a node reference used for events
	nodeRef := &v1.ObjectReference{
		Kind:      "Node",
		Name:      string(nodeName),
		UID:       types.UID(nodeName),
		Namespace: "",
	}

	oomWatcher, err := oomwatcher.NewWatcher(kubeDeps.Recorder)
	if err != nil {
		if libcontaineruserns.RunningInUserNS() {
			if utilfeature.DefaultFeatureGate.Enabled(features.KubeletInUserNamespace) {
				// oomwatcher.NewWatcher returns "open /dev/kmsg: operation not permitted" error,
				// when running in a user namespace with sysctl value `kernel.dmesg_restrict=1`.
				klog.V(2).InfoS("Failed to create an oomWatcher (running in UserNS, ignoring)", "err", err)
				oomWatcher = nil
			} else {
				klog.ErrorS(err, "Failed to create an oomWatcher (running in UserNS, Hint: enable KubeletInUserNamespace feature flag to ignore the error)")
				return nil, err
			}
		} else {
			return nil, err
		}
	}

	clusterDNS := make([]net.IP, 0, len(kubeCfg.ClusterDNS))
	for _, ipEntry := range kubeCfg.ClusterDNS {
		ip := netutils.ParseIPSloppy(ipEntry)
		if ip == nil {
			klog.InfoS("Invalid clusterDNS IP", "IP", ipEntry)
		} else {
			clusterDNS = append(clusterDNS, ip)
		}
	}
	httpClient := &http.Client{}

	klet := &Kubelet{
		hostname:                                hostname,
		hostnameOverridden:                      hostnameOverridden,
		nodeName:                                nodeName,
		kubeClient:                              kubeDeps.KubeClient,
		heartbeatClient:                         kubeDeps.HeartbeatClient,
		onRepeatedHeartbeatFailure:              kubeDeps.OnHeartbeatFailure,
		rootDirectory:                           rootDirectory,
		resyncInterval:                          kubeCfg.SyncFrequency.Duration,
		sourcesReady:                            config.NewSourcesReady(kubeDeps.PodConfig.SeenAllSources),
		registerNode:                            registerNode,
		registerWithTaints:                      registerWithTaints,
		registerSchedulable:                     registerSchedulable,
		dnsConfigurer:                           dns.NewConfigurer(kubeDeps.Recorder, nodeRef, nodeIPs, clusterDNS, kubeCfg.ClusterDomain, kubeCfg.ResolverConfig),
		serviceLister:                           serviceLister,
		serviceHasSynced:                        serviceHasSynced,
		nodeLister:                              nodeLister,
		nodeHasSynced:                           nodeHasSynced,
		masterServiceNamespace:                  masterServiceNamespace,
		streamingConnectionIdleTimeout:          kubeCfg.StreamingConnectionIdleTimeout.Duration,
		recorder:                                kubeDeps.Recorder,
		cadvisor:                                kubeDeps.CAdvisorInterface,
		cloud:                                   kubeDeps.Cloud,
		externalCloudProvider:                   cloudprovider.IsExternal(cloudProvider),
		providerID:                              providerID,
		nodeRef:                                 nodeRef,
		nodeLabels:                              nodeLabels,
		nodeStatusUpdateFrequency:               kubeCfg.NodeStatusUpdateFrequency.Duration,
		nodeStatusReportFrequency:               kubeCfg.NodeStatusReportFrequency.Duration,
		os:                                      kubeDeps.OSInterface,
		oomWatcher:                              oomWatcher,
		cgroupsPerQOS:                           kubeCfg.CgroupsPerQOS,
		cgroupRoot:                              kubeCfg.CgroupRoot,
		mounter:                                 kubeDeps.Mounter,
		hostutil:                                kubeDeps.HostUtil,
		subpather:                               kubeDeps.Subpather,
		maxPods:                                 int(kubeCfg.MaxPods),
		podsPerCore:                             int(kubeCfg.PodsPerCore),
		syncLoopMonitor:                         atomic.Value{},
		daemonEndpoints:                         daemonEndpoints,
		containerManager:                        kubeDeps.ContainerManager,
		containerRuntimeName:                    containerRuntime,
		nodeIPs:                                 nodeIPs,
		nodeIPValidator:                         validateNodeIP,
		clock:                                   clock.RealClock{},
		enableControllerAttachDetach:            kubeCfg.EnableControllerAttachDetach,
		makeIPTablesUtilChains:                  kubeCfg.MakeIPTablesUtilChains,
		iptablesMasqueradeBit:                   int(kubeCfg.IPTablesMasqueradeBit),
		iptablesDropBit:                         int(kubeCfg.IPTablesDropBit),
		experimentalHostUserNamespaceDefaulting: utilfeature.DefaultFeatureGate.Enabled(features.ExperimentalHostUserNamespaceDefaultingGate),
		keepTerminatedPodVolumes:                keepTerminatedPodVolumes,
		nodeStatusMaxImages:                     nodeStatusMaxImages,
		lastContainerStartedTime:                newTimeCache(),
	}

	if klet.cloud != nil {
		klet.cloudResourceSyncManager = cloudresource.NewSyncManager(klet.cloud, nodeName, klet.nodeStatusUpdateFrequency)
	}

	var secretManager secret.Manager
	var configMapManager configmap.Manager
	switch kubeCfg.ConfigMapAndSecretChangeDetectionStrategy {
	case kubeletconfiginternal.WatchChangeDetectionStrategy:
		secretManager = secret.NewWatchingSecretManager(kubeDeps.KubeClient, klet.resyncInterval)
		configMapManager = configmap.NewWatchingConfigMapManager(kubeDeps.KubeClient, klet.resyncInterval)
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
		klog.InfoS("Experimental host user namespace defaulting is enabled")
	}

	machineInfo, err := klet.cadvisor.MachineInfo()
	if err != nil {
		return nil, err
	}
	// Avoid collector collects it as a timestamped metric
	// See PR #95210 and #97006 for more details.
	machineInfo.Timestamp = time.Time{}
	klet.setCachedMachineInfo(machineInfo)

	imageBackOff := flowcontrol.NewBackOff(backOffPeriod, MaxContainerBackOff)

	klet.livenessManager = proberesults.NewManager()
	klet.readinessManager = proberesults.NewManager()
	klet.startupManager = proberesults.NewManager()
	klet.podCache = kubecontainer.NewCache()

	// podManager is also responsible for keeping secretManager and configMapManager contents up-to-date.
	mirrorPodClient := kubepod.NewBasicMirrorClient(klet.kubeClient, string(nodeName), nodeLister)
	klet.podManager = kubepod.NewBasicPodManager(mirrorPodClient, secretManager, configMapManager)

	klet.statusManager = status.NewManager(klet.kubeClient, klet.podManager, klet)

	klet.resourceAnalyzer = serverstats.NewResourceAnalyzer(klet, kubeCfg.VolumeStatsAggPeriod.Duration, kubeDeps.Recorder)

	klet.dockerLegacyService = kubeDeps.dockerLegacyService
	klet.runtimeService = kubeDeps.RemoteRuntimeService

	if kubeDeps.KubeClient != nil {
		klet.runtimeClassManager = runtimeclass.NewManager(kubeDeps.KubeClient)
	}

	if containerRuntime == kubetypes.RemoteContainerRuntime {
		// setup containerLogManager for CRI container runtime
		containerLogManager, err := logs.NewContainerLogManager(
			klet.runtimeService,
			kubeDeps.OSInterface,
			kubeCfg.ContainerLogMaxSize,
			int(kubeCfg.ContainerLogMaxFiles),
		)
		if err != nil {
			return nil, fmt.Errorf("failed to initialize container log manager: %v", err)
		}
		klet.containerLogManager = containerLogManager
	} else {
		klet.containerLogManager = logs.NewStubContainerLogManager()
	}

	klet.reasonCache = NewReasonCache()
	klet.workQueue = queue.NewBasicWorkQueue(klet.clock)
	klet.podWorkers = newPodWorkers(
		klet.syncPod,
		klet.syncTerminatingPod,
		klet.syncTerminatedPod,

		kubeDeps.Recorder,
		klet.workQueue,
		klet.resyncInterval,
		backOffPeriod,
		klet.podCache,
	)

	runtime, err := kuberuntime.NewKubeGenericRuntimeManager(
		kubecontainer.FilterEventRecorder(kubeDeps.Recorder),
		klet.livenessManager,
		klet.readinessManager,
		klet.startupManager,
		rootDirectory,
		machineInfo,
		klet.podWorkers,
		kubeDeps.OSInterface,
		klet,
		httpClient,
		imageBackOff,
		kubeCfg.SerializeImagePulls,
		float32(kubeCfg.RegistryPullQPS),
		int(kubeCfg.RegistryBurst),
		imageCredentialProviderConfigFile,
		imageCredentialProviderBinDir,
		kubeCfg.CPUCFSQuota,
		kubeCfg.CPUCFSQuotaPeriod,
		kubeDeps.RemoteRuntimeService,
		kubeDeps.RemoteImageService,
		kubeDeps.ContainerManager.InternalContainerLifecycle(),
		kubeDeps.dockerLegacyService,
		klet.containerLogManager,
		klet.runtimeClassManager,
		seccompDefault,
		kubeCfg.MemorySwap.SwapBehavior,
		kubeDeps.ContainerManager.GetNodeAllocatableAbsolute,
		*kubeCfg.MemoryThrottlingFactor,
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

	// common provider to get host file system usage associated with a pod managed by kubelet
	hostStatsProvider := stats.NewHostStatsProvider(kubecontainer.RealOS{}, func(podUID types.UID) (string, bool) {
		return getEtcHostsPath(klet.getPodDir(podUID)), klet.containerRuntime.SupportsSingleFileMapping()
	})
	if kubeDeps.useLegacyCadvisorStats {
		klet.StatsProvider = stats.NewCadvisorStatsProvider(
			klet.cadvisor,
			klet.resourceAnalyzer,
			klet.podManager,
			klet.runtimeCache,
			klet.containerRuntime,
			klet.statusManager,
			hostStatsProvider)
	} else {
		klet.StatsProvider = stats.NewCRIStatsProvider(
			klet.cadvisor,
			klet.resourceAnalyzer,
			klet.podManager,
			klet.runtimeCache,
			kubeDeps.RemoteRuntimeService,
			kubeDeps.RemoteImageService,
			hostStatsProvider,
			utilfeature.DefaultFeatureGate.Enabled(features.DisableAcceleratorUsageMetrics),
			utilfeature.DefaultFeatureGate.Enabled(features.PodAndContainerStatsFromCRI))
	}

	klet.pleg = pleg.NewGenericPLEG(klet.containerRuntime, plegChannelCapacity, plegRelistPeriod, klet.podCache, clock.RealClock{})
	klet.runtimeState = newRuntimeState(maxWaitForContainerRuntime)
	klet.runtimeState.addHealthCheck("PLEG", klet.pleg.Healthy)
	if _, err := klet.updatePodCIDR(kubeCfg.PodCIDR); err != nil {
		klog.ErrorS(err, "Pod CIDR update failed")
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
		klet.readinessManager,
		klet.startupManager,
		klet.runner,
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
		klet.getPluginsRegistrationDir(), /* sockDir */
		kubeDeps.Recorder,
	)

	// If the experimentalMounterPathFlag is set, we do not want to
	// check node capabilities since the mount path is not the default
	if len(experimentalMounterPath) != 0 {
		experimentalCheckNodeCapabilitiesBeforeMount = false
		// Replace the nameserver in containerized-mounter's rootfs/etc/resolve.conf with kubelet.ClusterDNS
		// so that service name could be resolved
		klet.dnsConfigurer.SetupDNSinContainerizedMounter(experimentalMounterPath)
	}

	// setup volumeManager
	klet.volumeManager = volumemanager.NewVolumeManager(
		kubeCfg.EnableControllerAttachDetach,
		nodeName,
		klet.podManager,
		klet.podWorkers,
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

	klet.backOff = flowcontrol.NewBackOff(backOffPeriod, MaxContainerBackOff)

	// setup eviction manager
	evictionManager, evictionAdmitHandler := eviction.NewManager(klet.resourceAnalyzer, evictionConfig, killPodNow(klet.podWorkers, kubeDeps.Recorder), klet.podManager.GetMirrorPodByPod, klet.imageManager, klet.containerGC, kubeDeps.Recorder, nodeRef, klet.clock)

	klet.evictionManager = evictionManager
	klet.admitHandlers.AddPodAdmitHandler(evictionAdmitHandler)

	// Safe, allowed sysctls can always be used as unsafe sysctls in the spec.
	// Hence, we concatenate those two lists.
	safeAndUnsafeSysctls := append(sysctlallowlist.SafeSysctlAllowlist(), allowedUnsafeSysctls...)
	sysctlsAllowlist, err := sysctl.NewAllowlist(safeAndUnsafeSysctls)
	if err != nil {
		return nil, err
	}
	klet.admitHandlers.AddPodAdmitHandler(sysctlsAllowlist)

	// enable active deadline handler
	activeDeadlineHandler, err := newActiveDeadlineHandler(klet.statusManager, kubeDeps.Recorder, klet.clock)
	if err != nil {
		return nil, err
	}
	klet.AddPodSyncLoopHandler(activeDeadlineHandler)
	klet.AddPodSyncHandler(activeDeadlineHandler)

	klet.admitHandlers.AddPodAdmitHandler(klet.containerManager.GetAllocateResourcesPodAdmitHandler())

	criticalPodAdmissionHandler := preemption.NewCriticalPodAdmissionHandler(klet.GetActivePods, killPodNow(klet.podWorkers, kubeDeps.Recorder), kubeDeps.Recorder)
	klet.admitHandlers.AddPodAdmitHandler(lifecycle.NewPredicateAdmitHandler(klet.getNodeAnyWay, criticalPodAdmissionHandler, klet.containerManager.UpdatePluginResources))
	// apply functional Option's
	for _, opt := range kubeDeps.Options {
		opt(klet)
	}

	if sysruntime.GOOS == "linux" {
		// AppArmor is a Linux kernel security module and it does not support other operating systems.
		klet.appArmorValidator = apparmor.NewValidator(containerRuntime)
		klet.softAdmitHandlers.AddPodAdmitHandler(lifecycle.NewAppArmorAdmitHandler(klet.appArmorValidator))
	}
	klet.softAdmitHandlers.AddPodAdmitHandler(lifecycle.NewNoNewPrivsAdmitHandler(klet.containerRuntime))
	klet.softAdmitHandlers.AddPodAdmitHandler(lifecycle.NewProcMountAdmitHandler(klet.containerRuntime))

	leaseDuration := time.Duration(kubeCfg.NodeLeaseDurationSeconds) * time.Second
	renewInterval := time.Duration(float64(leaseDuration) * nodeLeaseRenewIntervalFraction)
	klet.nodeLeaseController = lease.NewController(
		klet.clock,
		klet.heartbeatClient,
		string(klet.nodeName),
		kubeCfg.NodeLeaseDurationSeconds,
		klet.onRepeatedHeartbeatFailure,
		renewInterval,
		v1.NamespaceNodeLease,
		util.SetNodeOwnerFunc(klet.heartbeatClient, string(klet.nodeName)))

	// setup node shutdown manager
	shutdownManager, shutdownAdmitHandler := nodeshutdown.NewManager(&nodeshutdown.Config{
		ProbeManager:                     klet.probeManager,
		Recorder:                         kubeDeps.Recorder,
		NodeRef:                          nodeRef,
		GetPodsFunc:                      klet.GetActivePods,
		KillPodFunc:                      killPodNow(klet.podWorkers, kubeDeps.Recorder),
		SyncNodeStatusFunc:               klet.syncNodeStatus,
		ShutdownGracePeriodRequested:     kubeCfg.ShutdownGracePeriod.Duration,
		ShutdownGracePeriodCriticalPods:  kubeCfg.ShutdownGracePeriodCriticalPods.Duration,
		ShutdownGracePeriodByPodPriority: kubeCfg.ShutdownGracePeriodByPodPriority,
	})
	klet.shutdownManager = shutdownManager
	klet.admitHandlers.AddPodAdmitHandler(shutdownAdmitHandler)

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
	runner kubecontainer.CommandRunner

	// cAdvisor used for container information.
	cadvisor cadvisor.Interface

	// Set to true to have the node register itself with the apiserver.
	registerNode bool
	// List of taints to add to a node object when the kubelet registers itself.
	registerWithTaints []v1.Taint
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
	// serviceHasSynced indicates whether services have been sync'd at least once.
	// Check this before trusting a response from the lister.
	serviceHasSynced cache.InformerSynced
	// nodeLister knows how to list nodes
	nodeLister corelisters.NodeLister
	// nodeHasSynced indicates whether nodes have been sync'd at least once.
	// Check this before trusting a response from the node lister.
	nodeHasSynced cache.InformerSynced
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
	livenessManager  proberesults.Manager
	readinessManager proberesults.Manager
	startupManager   proberesults.Manager

	// How long to keep idle streaming command execution/port forwarding
	// connections open before terminating them
	streamingConnectionIdleTimeout time.Duration

	// The EventRecorder to use
	recorder record.EventRecorder

	// Policy for handling garbage collection of dead containers.
	containerGC kubecontainer.GC

	// Manager for image garbage collection.
	imageManager images.ImageGCManager

	// Manager for container logs.
	containerLogManager logs.ContainerLogManager

	// Secret manager.
	secretManager secret.Manager

	// ConfigMap manager.
	configMapManager configmap.Manager

	// Cached MachineInfo returned by cadvisor.
	machineInfoLock sync.RWMutex
	machineInfo     *cadvisorapi.MachineInfo

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

	// Container runtime.
	containerRuntime kubecontainer.Runtime

	// Streaming runtime handles container streaming.
	streamingRuntime kubecontainer.StreamingRuntime

	// Container runtime service (needed by container runtime Start()).
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

	// lastContainerStartedTime is the time of the last ContainerStarted event observed per pod
	lastContainerStartedTime *timeCache

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
	nodeLeaseController lease.Controller

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

	// Information about the ports which are opened by daemons on Node running this Kubelet server.
	daemonEndpoints *v1.NodeDaemonEndpoints

	// A queue used to trigger pod workers.
	workQueue queue.WorkQueue

	// oneTimeInitializer is used to initialize modules that are dependent on the runtime to be up.
	oneTimeInitializer sync.Once

	// If set, use this IP address or addresses for the node
	nodeIPs []net.IP

	// use this function to validate the kubelet nodeIP
	nodeIPValidator func(net.IP) error

	// If non-nil, this is a unique identifier for the node in an external database, eg. cloudprovider
	providerID string

	// clock is an interface that provides time related functionality in a way that makes it
	// easy to test the code.
	clock clock.WithTicker

	// handlers called during the tryUpdateNodeStatus cycle
	setNodeStatusFuncs []func(*v1.Node) error

	lastNodeUnschedulableLock sync.Mutex
	// maintains Node.Spec.Unschedulable value from previous run of tryUpdateNodeStatus()
	lastNodeUnschedulable bool

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

	// experimentalHostUserNamespaceDefaulting sets userns=true when users request host namespaces (pid, ipc, net),
	// are using non-namespaced capabilities (mknod, sys_time, sys_module), the pod contains a privileged container,
	// or using host path volumes.
	// This should only be enabled when the container runtime is performing user remapping AND if the
	// experimental behavior is desired.
	experimentalHostUserNamespaceDefaulting bool

	// dockerLegacyService contains some legacy methods for backward compatibility.
	// It should be set only when docker is using non json-file logging driver.
	dockerLegacyService legacy.DockerLegacyService

	// StatsProvider provides the node and the container stats.
	StatsProvider *stats.Provider

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

	// Handles node shutdown events for the Node.
	shutdownManager nodeshutdown.Manager
}

// ListPodStats is delegated to StatsProvider, which implements stats.Provider interface
func (kl *Kubelet) ListPodStats() ([]statsapi.PodStats, error) {
	return kl.StatsProvider.ListPodStats()
}

// ListPodCPUAndMemoryStats is delegated to StatsProvider, which implements stats.Provider interface
func (kl *Kubelet) ListPodCPUAndMemoryStats() ([]statsapi.PodStats, error) {
	return kl.StatsProvider.ListPodCPUAndMemoryStats()
}

// ListPodStatsAndUpdateCPUNanoCoreUsage is delegated to StatsProvider, which implements stats.Provider interface
func (kl *Kubelet) ListPodStatsAndUpdateCPUNanoCoreUsage() ([]statsapi.PodStats, error) {
	return kl.StatsProvider.ListPodStatsAndUpdateCPUNanoCoreUsage()
}

// ImageFsStats is delegated to StatsProvider, which implements stats.Provider interface
func (kl *Kubelet) ImageFsStats() (*statsapi.FsStats, error) {
	return kl.StatsProvider.ImageFsStats()
}

// GetCgroupStats is delegated to StatsProvider, which implements stats.Provider interface
func (kl *Kubelet) GetCgroupStats(cgroupName string, updateStats bool) (*statsapi.ContainerStats, *statsapi.NetworkStats, error) {
	return kl.StatsProvider.GetCgroupStats(cgroupName, updateStats)
}

// GetCgroupCPUAndMemoryStats is delegated to StatsProvider, which implements stats.Provider interface
func (kl *Kubelet) GetCgroupCPUAndMemoryStats(cgroupName string, updateStats bool) (*statsapi.ContainerStats, error) {
	return kl.StatsProvider.GetCgroupCPUAndMemoryStats(cgroupName, updateStats)
}

// RootFsStats is delegated to StatsProvider, which implements stats.Provider interface
func (kl *Kubelet) RootFsStats() (*statsapi.FsStats, error) {
	return kl.StatsProvider.RootFsStats()
}

// GetContainerInfo is delegated to StatsProvider, which implements stats.Provider interface
func (kl *Kubelet) GetContainerInfo(podFullName string, uid types.UID, containerName string, req *cadvisorapi.ContainerInfoRequest) (*cadvisorapi.ContainerInfo, error) {
	return kl.StatsProvider.GetContainerInfo(podFullName, uid, containerName, req)
}

// GetRawContainerInfo is delegated to StatsProvider, which implements stats.Provider interface
func (kl *Kubelet) GetRawContainerInfo(containerName string, req *cadvisorapi.ContainerInfoRequest, subcontainers bool) (map[string]*cadvisorapi.ContainerInfo, error) {
	return kl.StatsProvider.GetRawContainerInfo(containerName, req, subcontainers)
}

// RlimitStats is delegated to StatsProvider, which implements stats.Provider interface
func (kl *Kubelet) RlimitStats() (*statsapi.RlimitStats, error) {
	return kl.StatsProvider.RlimitStats()
}

// setupDataDirs creates:
// 1.  the root directory
// 2.  the pods directory
// 3.  the plugins directory
// 4.  the pod-resources directory
func (kl *Kubelet) setupDataDirs() error {
	kl.rootDirectory = path.Clean(kl.rootDirectory)
	pluginRegistrationDir := kl.getPluginsRegistrationDir()
	pluginsDir := kl.getPluginsDir()
	if err := os.MkdirAll(kl.getRootDir(), 0750); err != nil {
		return fmt.Errorf("error creating root directory: %v", err)
	}
	if err := kl.hostutil.MakeRShared(kl.getRootDir()); err != nil {
		return fmt.Errorf("error configuring root directory: %v", err)
	}
	if err := os.MkdirAll(kl.getPodsDir(), 0750); err != nil {
		return fmt.Errorf("error creating pods directory: %v", err)
	}
	if err := os.MkdirAll(kl.getPluginsDir(), 0750); err != nil {
		return fmt.Errorf("error creating plugins directory: %v", err)
	}
	if err := os.MkdirAll(kl.getPluginsRegistrationDir(), 0750); err != nil {
		return fmt.Errorf("error creating plugins registry directory: %v", err)
	}
	if err := os.MkdirAll(kl.getPodResourcesDir(), 0750); err != nil {
		return fmt.Errorf("error creating podresources directory: %v", err)
	}
	if selinux.SELinuxEnabled() {
		err := selinux.SetFileLabel(pluginRegistrationDir, config.KubeletPluginsDirSELinuxLabel)
		if err != nil {
			klog.InfoS("Unprivileged containerized plugins might not work, could not set selinux context on plugin registration dir", "path", pluginRegistrationDir, "err", err)
		}
		err = selinux.SetFileLabel(pluginsDir, config.KubeletPluginsDirSELinuxLabel)
		if err != nil {
			klog.InfoS("Unprivileged containerized plugins might not work, could not set selinux context on plugins dir", "path", pluginsDir, "err", err)
		}
	}
	return nil
}

// StartGarbageCollection starts garbage collection threads.
func (kl *Kubelet) StartGarbageCollection() {
	loggedContainerGCFailure := false
	// 每一分钟执行容器的gc
	go wait.Until(func() {
		if err := kl.containerGC.GarbageCollect(); err != nil {
			klog.ErrorS(err, "Container garbage collection failed")
			kl.recorder.Eventf(kl.nodeRef, v1.EventTypeWarning, events.ContainerGCFailed, err.Error())
			loggedContainerGCFailure = true
		} else {
			var vLevel klog.Level = 4
			if loggedContainerGCFailure {
				vLevel = 1
				loggedContainerGCFailure = false
			}

			klog.V(vLevel).InfoS("Container garbage collection succeeded")
		}
	}, ContainerGCPeriod, wait.NeverStop)

	// when the high threshold is set to 100, stub the image GC manager
	if kl.kubeletConfiguration.ImageGCHighThresholdPercent == 100 {
		klog.V(2).InfoS("ImageGCHighThresholdPercent is set 100, Disable image GC")
		return
	}

	prevImageGCFailed := false
	go wait.Until(func() {
		if err := kl.imageManager.GarbageCollect(); err != nil {
			if prevImageGCFailed {
				klog.ErrorS(err, "Image garbage collection failed multiple times in a row")
				// Only create an event for repeated failures
				kl.recorder.Eventf(kl.nodeRef, v1.EventTypeWarning, events.ImageGCFailed, err.Error())
			} else {
				klog.ErrorS(err, "Image garbage collection failed once. Stats initialization may not have completed yet")
			}
			prevImageGCFailed = true
		} else {
			var vLevel klog.Level = 4
			if prevImageGCFailed {
				vLevel = 1
				prevImageGCFailed = false
			}

			klog.V(vLevel).InfoS("Image garbage collection succeeded")
		}
	}, ImageGCPeriod, wait.NeverStop)
}

// initializeModules will initialize internal modules that do not require the container runtime to be up.
// Note that the modules here must not depend on modules that are not initialized here.
func (kl *Kubelet) initializeModules() error {
	// Prometheus metrics.
	metrics.Register(
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
	if _, err := os.Stat(ContainerLogsDir); err != nil {
		if err := kl.os.MkdirAll(ContainerLogsDir, 0755); err != nil {
			return fmt.Errorf("failed to create directory %q: %v", ContainerLogsDir, err)
		}
	}

	// Start the image manager.
	kl.imageManager.Start()

	// Start the certificate manager if it was enabled.
	if kl.serverCertificateManager != nil {
		kl.serverCertificateManager.Start()
	}

	// Start out of memory watcher.
	if kl.oomWatcher != nil {
		if err := kl.oomWatcher.Start(kl.nodeRef); err != nil {
			return fmt.Errorf("failed to start OOM watcher: %w", err)
		}
	}

	// Start resource analyzer
	kl.resourceAnalyzer.Start()

	return nil
}

// initializeRuntimeDependentModules will initialize internal modules that require the container runtime to be up.
func (kl *Kubelet) initializeRuntimeDependentModules() {
	if err := kl.cadvisor.Start(); err != nil {
		// Fail kubelet and rely on the babysitter to retry starting kubelet.
		klog.ErrorS(err, "Failed to start cAdvisor")
		os.Exit(1)
	}

	// trigger on-demand stats collection once so that we have capacity information for ephemeral storage.
	// ignore any errors, since if stats collection is not successful, the container manager will fail to start below.
	kl.StatsProvider.GetCgroupStats("/", true)
	// Start container manager.
	node, err := kl.getNodeAnyWay()
	if err != nil {
		// Fail kubelet and rely on the babysitter to retry starting kubelet.
		klog.ErrorS(err, "Kubelet failed to get node info")
		os.Exit(1)
	}
	// containerManager must start after cAdvisor because it needs filesystem capacity information
	if err := kl.containerManager.Start(node, kl.GetActivePods, kl.sourcesReady, kl.statusManager, kl.runtimeService); err != nil {
		// Fail kubelet and rely on the babysitter to retry starting kubelet.
		klog.ErrorS(err, "Failed to start ContainerManager")
		os.Exit(1)
	}
	// eviction manager must start after cadvisor because it needs to know if the container runtime has a dedicated imagefs
	kl.evictionManager.Start(kl.StatsProvider, kl.GetActivePods, kl.podResourcesAreReclaimed, evictionMonitoringPeriod)

	// container log manager must start after container runtime is up to retrieve information from container runtime
	// and inform container to reopen log file after log rotation.
	kl.containerLogManager.Start()
	// Adding Registration Callback function for CSI Driver
	kl.pluginManager.AddHandler(pluginwatcherapi.CSIPlugin, plugincache.PluginHandler(csi.PluginHandler))
	// Adding Registration Callback function for Device Manager
	kl.pluginManager.AddHandler(pluginwatcherapi.DevicePlugin, kl.containerManager.GetPluginRegistrationHandler())
	// Start the plugin manager
	klog.V(4).InfoS("Starting plugin manager")
	go kl.pluginManager.Run(kl.sourcesReady, wait.NeverStop)

	err = kl.shutdownManager.Start()
	if err != nil {
		// The shutdown manager is not critical for kubelet, so log failure, but don't block Kubelet startup if there was a failure starting it.
		klog.ErrorS(err, "Failed to start node shutdown manager")
	}
}

// Run starts the kubelet reacting to config updates
func (kl *Kubelet) Run(updates <-chan kubetypes.PodUpdate) {
	if kl.logServer == nil {
		kl.logServer = http.StripPrefix("/logs/", http.FileServer(http.Dir("/var/log/")))
	}
	if kl.kubeClient == nil {
		klog.InfoS("No API server defined - no node status update will be sent")
	}

	// Start the cloud provider sync manager
	if kl.cloudResourceSyncManager != nil {
		go kl.cloudResourceSyncManager.Run(wait.NeverStop)
	}

	if err := kl.initializeModules(); err != nil {
		kl.recorder.Eventf(kl.nodeRef, v1.EventTypeWarning, events.KubeletSetupFailed, err.Error())
		klog.ErrorS(err, "Failed to initialize internal modules")
		os.Exit(1)
	}

	// Start volume manager
	go kl.volumeManager.Run(kl.sourcesReady, wait.NeverStop)

	if kl.kubeClient != nil {
		// Introduce some small jittering to ensure that over time the requests won't start
		// accumulating at approximately the same time from the set of nodes due to priority and
		// fairness effect.
		go wait.JitterUntil(kl.syncNodeStatus, kl.nodeStatusUpdateFrequency, 0.04, true, wait.NeverStop)
		go kl.fastStatusUpdateOnce()

		// start syncing lease
		go kl.nodeLeaseController.Run(wait.NeverStop)
	}
	go wait.Until(kl.updateRuntimeUp, 5*time.Second, wait.NeverStop)

	// Set up iptables util rules
	if kl.makeIPTablesUtilChains {
		kl.initNetworkUtil()
	}

	// Start component sync loops.
	kl.statusManager.Start()

	// Start syncing RuntimeClasses if enabled.
	if kl.runtimeClassManager != nil {
		kl.runtimeClassManager.Start(wait.NeverStop)
	}

	// Start the pod lifecycle event generator.
	kl.pleg.Start()
	kl.syncLoop(updates, kl)
}

// syncPod is the transaction script for the sync of a single pod (setting up)
// a pod. This method is reentrant and expected to converge a pod towards the
// desired state of the spec. The reverse (teardown) is handled in
// syncTerminatingPod and syncTerminatedPod. If syncPod exits without error,
// then the pod runtime state is in sync with the desired configuration state
// (pod is running). If syncPod exits with a transient error, the next
// invocation of syncPod is expected to make progress towards reaching the
// runtime state. syncPod exits with isTerminal when the pod was detected to
// have reached a terminal lifecycle phase due to container exits (for
// RestartNever or RestartOnFailure) and the next method invoked will by
// syncTerminatingPod.
//
// Arguments:
//
// updateType - whether this is a create (first time) or an update, should
//
//	only be used for metrics since this method must be reentrant
//
// pod - the pod that is being set up
// mirrorPod - the mirror pod known to the kubelet for this pod, if any
// podStatus - the most recent pod status observed for this pod which can
//
//	be used to determine the set of actions that should be taken during
//	this loop of syncPod
//
// The workflow is:
//   - If the pod is being created, record pod worker start latency
//   - Call generateAPIPodStatus to prepare an v1.PodStatus for the pod
//   - If the pod is being seen as running for the first time, record pod
//     start latency
//   - Update the status of the pod in the status manager
//   - Stop the pod's containers if it should not be running due to soft
//     admission
//   - Ensure any background tracking for a runnable pod is started
//   - Create a mirror pod if the pod is a static pod, and does not
//     already have a mirror pod
//   - Create the data directories for the pod if they do not exist
//   - Wait for volumes to attach/mount
//   - Fetch the pull secrets for the pod
//   - Call the container runtime's SyncPod callback
//   - Update the traffic shaping for the pod's ingress and egress limits
//
// If any step of this workflow errors, the error is returned, and is repeated
// on the next syncPod call.
//
// This operation writes all events that are dispatched in order to provide
// the most accurate information possible about an error situation to aid debugging.
// Callers should not write an event if this operation returns an error.
func (kl *Kubelet) syncPod(ctx context.Context, updateType kubetypes.SyncPodType, pod, mirrorPod *v1.Pod, podStatus *kubecontainer.PodStatus) (isTerminal bool, err error) {
	klog.V(4).InfoS("syncPod enter", "pod", klog.KObj(pod), "podUID", pod.UID)
	defer func() {
		klog.V(4).InfoS("syncPod exit", "pod", klog.KObj(pod), "podUID", pod.UID, "isTerminal", isTerminal)
	}()

	// Latency measurements for the main workflow are relative to the
	// first time the pod was seen by the API server.
	var firstSeenTime time.Time
	// pod的annotations["kubernetes.io/config.seen"]
	if firstSeenTimeStr, ok := pod.Annotations[kubetypes.ConfigFirstSeenAnnotationKey]; ok {
		firstSeenTime = kubetypes.ConvertToTimestamp(firstSeenTimeStr).Get()
	}

	// Record pod worker start latency if being created
	// TODO: make pod workers record their own latencies
	// type为kubetypes.SyncPodCreate且pod的annotations["kubernetes.io/config.seen"]不为0值，则记录metrics
	if updateType == kubetypes.SyncPodCreate {
		if !firstSeenTime.IsZero() {
			// This is the first time we are syncing the pod. Record the latency
			// since kubelet first saw the pod if firstSeenTime is set.
			metrics.PodWorkerStartDuration.Observe(metrics.SinceInSeconds(firstSeenTime))
		} else {
			klog.V(3).InfoS("First seen time not recorded for pod",
				"podUID", pod.UID,
				"pod", klog.KObj(pod))
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

	// If the pod is terminal, we don't need to continue to setup the pod
	// pod的phase处于"Succeeded"或"Failed"状态，
	// 则将status规整化后，与内部的kl.statusManager.podStatuses进行比较，发生变化，则发送到kl.statusManager.podStatusChannel进行更新或等待批量更新
	// 返回true，nil
	if apiPodStatus.Phase == v1.PodSucceeded || apiPodStatus.Phase == v1.PodFailed {
		kl.statusManager.SetPodStatus(pod, apiPodStatus)
		isTerminal = true
		return isTerminal, nil
	}

	// If the pod should not be running, we request the pod's containers be stopped. This is not the same
	// as termination (we want to stop the pod, but potentially restart it later if soft admission allows
	// it later). Set the status and phase appropriately
	// kl.softAdmitHandlers里的handler是否都admit通过
	runnable := kl.canRunPod(pod)
	// admit不通过
	if !runnable.Admit {
		// Pod is not runnable; and update the Pod and Container statuses to why.
		// pod的phase不为Failed和Successed（"Running"或"Pending"或"Unknown"），则pod的phase设置为"Pending"
		if apiPodStatus.Phase != v1.PodFailed && apiPodStatus.Phase != v1.PodSucceeded {
			apiPodStatus.Phase = v1.PodPending
		}
		// 设置pod的Reason和Message
		apiPodStatus.Reason = runnable.Reason
		apiPodStatus.Message = runnable.Message
		// Waiting containers are not creating.
		const waitingReason = "Blocked"
		for _, cs := range apiPodStatus.InitContainerStatuses {
			// 当initcontainer处于Waiting状态，设置container的status的Reason为"Blocked"
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

	// Record the time it takes for the pod to become running.
	// 从kl.statusManager.podStatuses缓存中获取uid的相关的pod的status，（如果是mirror pod uid则通过static pod uid来查找）
	existingStatus, ok := kl.statusManager.GetPodStatus(pod.UID)
	// statusManager里不存在这个pod的status，或statusManager（apiserver）中phase为"Pending"且runtime中的pod的status里Phase为"Running"且kubelet已经见到这个pod
	// 即apiserver中的状态和runtime中的不一样，pod已经启动了，则激励pod启动时间
	if !ok || existingStatus.Phase == v1.PodPending && apiPodStatus.Phase == v1.PodRunning &&
		!firstSeenTime.IsZero() {
		metrics.PodStartDuration.Observe(metrics.SinceInSeconds(firstSeenTime))
	}

	// 同步最新pod status到apiserver
	// 将status规整化后，与内部的kl.statusManager.podStatuses进行比较，发生变化，则发送到kl.statusManager.podStatusChannel进行更新或等待批量更新
	kl.statusManager.SetPodStatus(pod, apiPodStatus)

	// Pods that are not runnable must be stopped - return a typed error to the pod worker
	// kl.softAdmitHandlers里的handler是否都admit通过（appArmor和NoNewPrivs和ProcMount）
	if !runnable.Admit {
		klog.V(2).InfoS("Pod is not runnable and must have running containers stopped", "pod", klog.KObj(pod), "podUID", pod.UID, "message", runnable.Message)
		var syncErr error
		// 从podStatus中返回pod正在运行的container和sandbox
		p := kubecontainer.ConvertPodStatusToRunningPod(kl.getRuntime().Type(), podStatus)
		// 1. 停止pod里所有的container，每个container都启动一个goroutine进行killContainer（执行prestop和stop container）
		// 2. 调用网络插件释放容器的网卡，顺序停止pod里所有的sandbox container
		// 3. 更新Burstable和BestEffort qos class的cpu、memory、hugepage的cgroup目录属性值
		if err := kl.killPod(pod, p, nil); err != nil {
			kl.recorder.Eventf(pod, v1.EventTypeWarning, events.FailedToKillPod, "error killing pod: %v", err)
			syncErr = fmt.Errorf("error killing pod: %v", err)
			utilruntime.HandleError(syncErr)
		} else {
			// There was no error killing the pod, but the pod cannot be run.
			// Return an error to signal that the sync loop should back off.
			syncErr = fmt.Errorf("pod cannot be run: %s", runnable.Message)
		}
		return false, syncErr
	}

	// If the network plugin is not ready, only start the pod if it uses the host network
	// 网络插件unready且pod不是host网络模式，返回错误
	// 即如果网络插件unready，则只运行host网络模式的pod。这个解决了鸡生蛋蛋生鸡问题（网络插件是daemonset运行）
	if err := kl.runtimeState.networkErrors(); err != nil && !kubecontainer.IsHostNetworkPod(pod) {
		kl.recorder.Eventf(pod, v1.EventTypeWarning, events.NetworkNotReady, "%s: %v", NetworkNotReadyErrorMsg, err)
		return false, fmt.Errorf("%s: %v", NetworkNotReadyErrorMsg, err)
	}

	// ensure the kubelet knows about referenced secrets or configmaps used by the pod
	// uid在p.podSyncStatuses里，podWorker不在terminating状态
	// 或uid不在p.podSyncStatuses里
	if !kl.podWorkers.IsPodTerminationRequested(pod.UID) {
		if kl.secretManager != nil {
			// 将pod添加到kl.secretManager.manager.registeredPods字段中，在kl.secretManager.manager.objectStore里增加相关secret的引用计数，如果这个对象不存在，则创建一个新的reflector（只watch和list这个资源）
			kl.secretManager.RegisterPod(pod)
		}
		if kl.configMapManager != nil {
			// 将pod添加到kl.secretManager.manager.registeredPods字段中，在kl.secretManager.manager.objectStore里增加相关configmap的引用计数，如果这个对象不存在，则创建一个新的reflector（只watch和list这个资源）
			kl.configMapManager.RegisterPod(pod)
		}
	}

	// Create Cgroups for the pod and apply resource parameters
	// to them if cgroups-per-qos flag is enabled.
	pcm := kl.containerManager.NewPodContainerManager()
	// If pod has already been terminated then we need not create
	// or update the pod's cgroup
	// TODO: once context cancellation is added this check can be removed
	// uid在p.podSyncStatuses里，podWorker不在terminating状态
	// 或uid不在p.podSyncStatuses里
	if !kl.podWorkers.IsPodTerminationRequested(pod.UID) {
		// When the kubelet is restarted with the cgroups-per-qos
		// flag enabled, all the pod's running containers
		// should be killed intermittently and brought back up
		// under the qos cgroup hierarchy.
		// Check if this is the pod's first sync
		firstSync := true
		// 至少有一个容器已经在运行，则不是第一次执行
		for _, containerStatus := range apiPodStatus.ContainerStatuses {
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
			// 从podStatus中返回pod正在运行的container和sandbox
			p := kubecontainer.ConvertPodStatusToRunningPod(kl.getRuntime().Type(), podStatus)
			// 1. 停止pod里所有的container，每个container都启动一个goroutine进行killContainer（执行prestop和stop container）
			// 2. 调用网络插件释放容器的网卡，顺序停止pod里所有的sandbox container
			// 3. 更新Burstable和BestEffort qos class的cpu、memory、hugepage的cgroup目录属性值
			if err := kl.killPod(pod, p, nil); err == nil {
				podKilled = true
			} else {
				klog.ErrorS(err, "KillPod failed", "pod", klog.KObj(pod), "podStatus", podStatus)
			}
		}
		// Create and Update pod's Cgroups
		// Don't create cgroups for run once pod if it was killed above
		// The current policy is not to restart the run once pods when
		// the kubelet is restarted with the new flag as run once pods are
		// expected to run only once and if the kubelet is restarted then
		// they are not expected to run again.
		// We don't create and apply updates to cgroup if its a run once pod and was killed above
		// pod没有被停止（第一次执行，或不是第一次执行且cgroup全都存在），或pod的RestartPolicy不为"Never"
		// 且至少有一个cgroup子系统中，pod的cgroup路径不存在，则创建cgroup目录和设置cgroup属性
		if !(podKilled && pod.Spec.RestartPolicy == v1.RestartPolicyNever) {
			// 至少有一个cgroup子系统中，pod的cgroup路径不存在
			if !pcm.Exists(pod) {
				if err := kl.containerManager.UpdateQOSCgroups(); err != nil {
					klog.V(2).InfoS("Failed to update QoS cgroups while syncing pod", "pod", klog.KObj(pod), "err", err)
				}
				// 在所有cgroup子系统，创建pod的cgroup路径，并设置相应cgroup（cpu、memory、pid、hugepage在系统中开启的）资源属性的限制
				if err := pcm.EnsureExists(pod); err != nil {
					kl.recorder.Eventf(pod, v1.EventTypeWarning, events.FailedToCreatePodContainer, "unable to ensure pod container exists: %v", err)
					return false, fmt.Errorf("failed to ensure that the pod: %v cgroups exist and are correctly applied: %v", pod.UID, err)
				}
			}
		}
	}

	// Create Mirror Pod for Static Pod if it doesn't already exist
	// 如果是static pod，则检查是否存在mirror pod，当mirror pod不存在时候进行创建
	if kubetypes.IsStaticPod(pod) {
		deleted := false
		// 存在mirror pod
		if mirrorPod != nil {
			// mirrorPod被删除或mirrorPod不是pod的mirror pod
			if mirrorPod.DeletionTimestamp != nil || !kl.podManager.IsMirrorPodOf(mirrorPod, pod) {
				// The mirror pod is semantically different from the static pod. Remove
				// it. The mirror pod will get recreated later.
				klog.InfoS("Trying to delete pod", "pod", klog.KObj(pod), "podUID", mirrorPod.ObjectMeta.UID)
				// pod.Name + "_" + pod.Namespace
				podFullName := kubecontainer.GetPodFullName(pod)
				var err error
				// 调用api删除mirror pod（从podFullName解析出name和namespace）
				deleted, err = kl.podManager.DeleteMirrorPod(podFullName, &mirrorPod.ObjectMeta.UID)
				if deleted {
					klog.InfoS("Deleted mirror pod because it is outdated", "pod", klog.KObj(mirrorPod))
				} else if err != nil {
					klog.ErrorS(err, "Failed deleting mirror pod", "pod", klog.KObj(mirrorPod))
				}
			}
		}
		// static pod没有mirror pod或mirror pod被删除，则在获取node未发生错误且node未被删除条件下，创建mirror pod
		if mirrorPod == nil || deleted {
			node, err := kl.GetNode()
			// 当获取node发生错误或node被删除，则无需创建mirror pod
			if err != nil || node.DeletionTimestamp != nil {
				klog.V(4).InfoS("No need to create a mirror pod, since node has been removed from the cluster", "node", klog.KRef("", string(kl.nodeName)))
			} else {
				klog.V(4).InfoS("Creating a mirror pod for static pod", "pod", klog.KObj(pod))
				// node没有被删除，则创建static pod的mirror pod
				if err := kl.podManager.CreateMirrorPod(pod); err != nil {
					klog.ErrorS(err, "Failed creating a mirror pod for", "pod", klog.KObj(pod))
				}
			}
		}
	}

	// Make data directories for the pod
	// 创建这个pod相关的目录（volumes和plugins）
	if err := kl.makePodDataDirs(pod); err != nil {
		kl.recorder.Eventf(pod, v1.EventTypeWarning, events.FailedToMakePodDataDirectories, "error making pod data directories: %v", err)
		klog.ErrorS(err, "Unable to make pod data directories for pod", "pod", klog.KObj(pod))
		return false, err
	}

	// Volume manager will not mount volumes for terminating pods
	// TODO: once context cancellation is added this check can be removed
	// uid在p.podSyncStatuses里，podWorker不在terminating状态
	// 或uid不在p.podSyncStatuses里
	if !kl.podWorkers.IsPodTerminationRequested(pod.UID) {
		// Wait for volumes to attach/mount
		// 在2m3s内等待pod的所有volume进行挂载
		if err := kl.volumeManager.WaitForAttachAndMount(pod); err != nil {
			kl.recorder.Eventf(pod, v1.EventTypeWarning, events.FailedMountVolume, "Unable to attach or mount volumes: %v", err)
			klog.ErrorS(err, "Unable to attach or mount volumes for pod; skipping pod", "pod", klog.KObj(pod))
			return false, err
		}
	}

	// Fetch the pull secrets for the pod
	// 获取pod相关的所有ImagePullSecrets
	pullSecrets := kl.getPullSecretsForPod(pod)

	// Ensure the pod is being probed
	// 启动pod里面所有设置了probe的container的probe
	kl.probeManager.AddPod(pod)

	// Call the container runtime's SyncPod callback
	// 1. 根据runtime种container和sandbox的状态，计算出需要采取的动作
	// 2. 执行相关的动作，Kill pod sandbox、Kill any containers、 Create sandbox、Create ephemeral containers、Create init containers、Create normal containers
	result := kl.containerRuntime.SyncPod(pod, podStatus, pullSecrets, kl.backOff)
	kl.reasonCache.Update(pod.UID, result)
	// 只更新"StartContainer"的动作的事件，如果result没有错误，则移除缓存中"{uid}_{target}"相关的错误，否则就增加"{uid}_{target}"的相关错误
	if err := result.Error(); err != nil {
		// Do not return error if the only failures were pods in backoff
		for _, r := range result.SyncResults {
			if r.Error != kubecontainer.ErrCrashLoopBackOff && r.Error != images.ErrImagePullBackOff {
				// Do not record an event here, as we keep all event logging for sync pod failures
				// local to container runtime, so we get better errors.
				return false, err
			}
		}

		return false, nil
	}

	return false, nil
}

// syncTerminatingPod is expected to terminate all running containers in a pod. Once this method
// returns without error, the pod's local state can be safely cleaned up. If runningPod is passed,
// we perform no status updates.
// 如果runningPod不为nil（可能pod被删了，但是还是存在于runtime，或pod删除又被创建）
//   1. 停止pod里所有的container，每个container都启动一个goroutine进行killContainer（执行prestop和stop container）
//   2. 调用网络插件释放容器的网卡，顺序停止pod里所有的sandbox container
//   3. 更新Burstable和BestEffort qos class的cpu、memory、hugepage的cgroup目录属性值
// 如果runningPod不为nil
//   1. 更新pod status（内部和apiserver）
//   2. 停止liveness和startup probes
//   3. 停止pod里所有的container，每个container都启动一个goroutine进行killContainer（执行prestop和stop container）
//   4. 调用网络插件释放容器的网卡，顺序停止pod里所有的sandbox container
//   5. 更新Burstable和BestEffort qos class的cpu、memory、hugepage的cgroup目录属性值
//   6. 停止readiness probe
func (kl *Kubelet) syncTerminatingPod(ctx context.Context, pod *v1.Pod, podStatus *kubecontainer.PodStatus, runningPod *kubecontainer.Pod, gracePeriod *int64, podStatusFn func(*v1.PodStatus)) error {
	klog.V(4).InfoS("syncTerminatingPod enter", "pod", klog.KObj(pod), "podUID", pod.UID)
	defer klog.V(4).InfoS("syncTerminatingPod exit", "pod", klog.KObj(pod), "podUID", pod.UID)

	// when we receive a runtime only pod (runningPod != nil) we don't need to update the status
	// manager or refresh the status of the cache, because a successful killPod will ensure we do
	// not get invoked again
	if runningPod != nil {
		// we kill the pod with the specified grace period since this is a termination
		if gracePeriod != nil {
			klog.V(4).InfoS("Pod terminating with grace period", "pod", klog.KObj(pod), "podUID", pod.UID, "gracePeriod", *gracePeriod)
		} else {
			klog.V(4).InfoS("Pod terminating with grace period", "pod", klog.KObj(pod), "podUID", pod.UID, "gracePeriod", nil)
		}
		// 1. 停止pod里所有的container，每个container都启动一个goroutine进行killContainer（执行prestop和stop container）
		// 2. 调用网络插件释放容器的网卡，顺序停止pod里所有的sandbox container
		// 3. 更新Burstable和BestEffort qos class的cpu、memory、hugepage的cgroup目录属性值
		if err := kl.killPod(pod, *runningPod, gracePeriod); err != nil {
			kl.recorder.Eventf(pod, v1.EventTypeWarning, events.FailedToKillPod, "error killing pod: %v", err)
			// there was an error killing the pod, so we return that error directly
			utilruntime.HandleError(err)
			return err
		}
		klog.V(4).InfoS("Pod termination stopped all running orphan containers", "pod", klog.KObj(pod), "podUID", pod.UID)
		return nil
	}

	// 根据runtime的status生成pod status
	apiPodStatus := kl.generateAPIPodStatus(pod, podStatus)
	if podStatusFn != nil {
		podStatusFn(&apiPodStatus)
	}
	// 将status规整化后，与内部的kl.statusManager.podStatuses进行比较，发生变化，则发送到kl.statusManager.podStatusChannel进行更新或等待批量更新
	kl.statusManager.SetPodStatus(pod, apiPodStatus)

	if gracePeriod != nil {
		klog.V(4).InfoS("Pod terminating with grace period", "pod", klog.KObj(pod), "podUID", pod.UID, "gracePeriod", *gracePeriod)
	} else {
		klog.V(4).InfoS("Pod terminating with grace period", "pod", klog.KObj(pod), "podUID", pod.UID, "gracePeriod", nil)
	}

	// 停止liveness和startup probes
	kl.probeManager.StopLivenessAndStartup(pod)

	// 从podStatus中返回pod正在运行的container和sandbox
	p := kubecontainer.ConvertPodStatusToRunningPod(kl.getRuntime().Type(), podStatus)
	// 1. 停止pod里所有的container，每个container都启动一个goroutine进行killContainer（执行prestop和stop container）
	// 2. 调用网络插件释放容器的网卡，顺序停止pod里所有的sandbox container
	// 3. 更新Burstable和BestEffort qos class的cpu、memory、hugepage的cgroup目录属性值
	if err := kl.killPod(pod, p, gracePeriod); err != nil {
		kl.recorder.Eventf(pod, v1.EventTypeWarning, events.FailedToKillPod, "error killing pod: %v", err)
		// there was an error killing the pod, so we return that error directly
		utilruntime.HandleError(err)
		return err
	}

	// Once the containers are stopped, we can stop probing for liveness and readiness.
	// TODO: once a pod is terminal, certain probes (liveness exec) could be stopped immediately after
	//   the detection of a container shutdown or (for readiness) after the first failure. Tracked as
	//   https://github.com/kubernetes/kubernetes/issues/107894 although may not be worth optimizing.
	// 停止readiness, liveness, startup probe
	kl.probeManager.RemovePod(pod)

	// Guard against consistency issues in KillPod implementations by checking that there are no
	// running containers. This method is invoked infrequently so this is effectively free and can
	// catch race conditions introduced by callers updating pod status out of order.
	// TODO: have KillPod return the terminal status of stopped containers and write that into the
	//  cache immediately
	// 直接从runtime中获得pod status（pod的uid、name、namespace、以及所有sandbox状态、ips、所有容器状态）
	podStatus, err := kl.containerRuntime.GetPodStatus(pod.UID, pod.Name, pod.Namespace)
	if err != nil {
		klog.ErrorS(err, "Unable to read pod status prior to final pod termination", "pod", klog.KObj(pod), "podUID", pod.UID)
		return err
	}
	var runningContainers []string
	var containers []string
	for _, s := range podStatus.ContainerStatuses {
		if s.State == kubecontainer.ContainerStateRunning {
			runningContainers = append(runningContainers, s.ID.String())
		}
		containers = append(containers, fmt.Sprintf("(%s state=%s exitCode=%d finishedAt=%s)", s.Name, s.State, s.ExitCode, s.FinishedAt.UTC().Format(time.RFC3339Nano)))
	}
	if klog.V(4).Enabled() {
		sort.Strings(containers)
		klog.InfoS("Post-termination container state", "pod", klog.KObj(pod), "podUID", pod.UID, "containers", strings.Join(containers, " "))
	}
	// 停止pod后还存在运行的container，返回错误
	if len(runningContainers) > 0 {
		return fmt.Errorf("detected running containers after a successful KillPod, CRI violation: %v", runningContainers)
	}

	// we have successfully stopped all containers, the pod is terminating, our status is "done"
	klog.V(4).InfoS("Pod termination stopped all running containers", "pod", klog.KObj(pod), "podUID", pod.UID)

	return nil
}

// syncTerminatedPod cleans up a pod that has terminated (has no running containers).
// The invocations in this call are expected to tear down what PodResourcesAreReclaimed checks (which
// gates pod deletion). When this method exits the pod is expected to be ready for cleanup.
// TODO: make this method take a context and exit early
// 根据runtime的status生成pod status，更新pod status（内部和apiserver）
// 等待pod的volume umount完成
// 在kl.secretManager.manager.objectCache减少pod相关secret的引用计数，相关的secret次数为0，则停止这secret的reflector
// 在kl.configMapManager.manager.objectCache减少pod相关configmap的引用计数，相关的configmap次数为0，则停止这configmap的reflector
// 如果kl.cgroupsPerQOS为true
//   kill podCgroup的cgroup子系统的cgroup路径下，所有attached pid（包括子cgroup下attached pid）
//   移除所有cgroup子系统里的cgroup路径
// 设置所有非Terminated或非Waiting的container status为Terminated状态，如果pod status发生变化，则更新pod status（内部和apiserver）
func (kl *Kubelet) syncTerminatedPod(ctx context.Context, pod *v1.Pod, podStatus *kubecontainer.PodStatus) error {
	klog.V(4).InfoS("syncTerminatedPod enter", "pod", klog.KObj(pod), "podUID", pod.UID)
	defer klog.V(4).InfoS("syncTerminatedPod exit", "pod", klog.KObj(pod), "podUID", pod.UID)

	// generate the final status of the pod
	// TODO: should we simply fold this into TerminatePod? that would give a single pod update
	// 根据runtime的status生成pod status
	apiPodStatus := kl.generateAPIPodStatus(pod, podStatus)
	// 将status规整化后，与内部的kl.statusManager.podStatuses进行比较，发生变化，则发送到kl.statusManager.podStatusChannel进行更新或等待批量更新
	kl.statusManager.SetPodStatus(pod, apiPodStatus)

	// volumes are unmounted after the pod worker reports ShouldPodRuntimeBeRemoved (which is satisfied
	// before syncTerminatedPod is invoked)
	// 等待pod的volume umount完成
	if err := kl.volumeManager.WaitForUnmount(pod); err != nil {
		return err
	}
	klog.V(4).InfoS("Pod termination unmounted volumes", "pod", klog.KObj(pod), "podUID", pod.UID)

	// After volume unmount is complete, let the secret and configmap managers know we're done with this pod
	if kl.secretManager != nil {
		// 在kl.secretManager.manager.objectCache减少pod相关secret的引用计数，相关的secret次数为0，则停止这secret的reflector
		kl.secretManager.UnregisterPod(pod)
	}
	if kl.configMapManager != nil {
		// 在kl.configMapManager.manager.objectCache减少pod相关configmap的引用计数，相关的configmap次数为0，则停止这configmap的reflector
		kl.configMapManager.UnregisterPod(pod)
	}

	// Note: we leave pod containers to be reclaimed in the background since dockershim requires the
	// container for retrieving logs and we want to make sure logs are available until the pod is
	// physically deleted.

	// remove any cgroups in the hierarchy for pods that are no longer running.
	// 默认为true
	if kl.cgroupsPerQOS {
		pcm := kl.containerManager.NewPodContainerManager()
		name, _ := pcm.GetPodContainerName(pod)
		// kill podCgroup的cgroup子系统的cgroup路径下，所有attached pid（包括子cgroup下attached pid）
		// 移除所有cgroup子系统里的cgroup路径
		if err := pcm.Destroy(name); err != nil {
			return err
		}
		klog.V(4).InfoS("Pod termination removed cgroups", "pod", klog.KObj(pod), "podUID", pod.UID)
	}

	// mark the final pod status
	// 设置所有非Terminated或非Waiting的container status为Terminated状态
	// 如果pod status发生变化，则更新podStatuses字段中的pod状态，并添加需要更新的status到podStatusChannel
	// 如果podStatusChannel满了，则直接放弃，等待周期性syncBatch进行同步
	kl.statusManager.TerminatePod(pod)
	klog.V(4).InfoS("Pod is terminated and will need no more status updates", "pod", klog.KObj(pod), "podUID", pod.UID)

	return nil
}

// Get pods which should be resynchronized. Currently, the following pod should be resynchronized:
//   - pod whose work is ready.
//   - internal modules that request sync of a pod.
// 从kl.podManager里所有static pod和普通的pod进行筛选，返回的pod包含
// 1. 这个pod uid是在pod worker中处理过的uid（执行完成的uid，包括已经执行成功和执行失败）
// 2. pod中还未被pod worker处理过，且kl.podSyncLoopHandler（返回true的）
// kl.podSyncLoopHandler只有一个pkg\kubelet\active_deadline.go activeDeadlineHandler
// activeDeadlineHandler
// 检测pod status中StartTime距现在时间长度，超出pod.Spec.ActiveDeadlineSeconds，返回true
// 即pod的active时间长度是否超过pod.Spec.ActiveDeadlineSeconds
func (kl *Kubelet) getPodsToSync() []*v1.Pod {
	// 获取所有pod包括static pod和普通的pod
	allPods := kl.podManager.GetPods()
	// 获得queue中过期（早于现在）的uid列表，并从队列中移除
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
		// pod中还未被pod worker处理过，让kl.podSyncLoopHandler决定是否
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
// 不是每种source至少有一个Pod（podUpdate事件）,返回错误
// 重新执行kl.podWorkers.UpdatePod，触发重新执行podWork（event类型为SyncPodKill）,让podWorker执行terminating（static pod文件被移除）或不做任何事情（podWorker已经finished）
func (kl *Kubelet) deletePod(pod *v1.Pod) error {
	if pod == nil {
		return fmt.Errorf("deletePod does not allow nil pod")
	}
	// 不是每种source至少有一个Pod（podUpdate事件）
	if !kl.sourcesReady.AllReady() {
		// If the sources aren't ready, skip deletion, as we may accidentally delete pods
		// for sources that haven't reported yet.
		return fmt.Errorf("skipping delete because sources aren't ready yet")
	}
	klog.V(3).InfoS("Pod has been deleted and must be killed", "pod", klog.KObj(pod), "podUID", pod.UID)
	// 重新执行kl.podWorkers.UpdatePod，触发重新执行podWork（event类型为SyncPodKill）,让podWorker执行terminating（static pod文件被移除）或不做任何事情（podWorker已经finished）
	kl.podWorkers.UpdatePod(UpdatePodOptions{
		Pod:        pod,
		UpdateType: kubetypes.SyncPodKill,
	})
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
	attrs.OtherPods = kl.GetActivePods()

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
	klog.InfoS("Starting kubelet main sync loop")
	// The syncTicker wakes up kubelet to checks if there are any pod workers
	// that need to be sync'd. A one-second period is sufficient because the
	// sync interval is defaulted to 10s.
	syncTicker := time.NewTicker(time.Second)
	defer syncTicker.Stop()
	housekeepingTicker := time.NewTicker(housekeepingPeriod)
	defer housekeepingTicker.Stop()
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
	if kl.dnsConfigurer != nil && kl.dnsConfigurer.ResolverConfig != "" {
		kl.dnsConfigurer.CheckLimitsForResolvConf()
	}

	for {
		if err := kl.runtimeState.runtimeErrors(); err != nil {
			klog.ErrorS(err, "Skipping pod synchronization")
			// exponential backoff
			time.Sleep(duration)
			duration = time.Duration(math.Min(float64(max), factor*float64(duration)))
			continue
		}
		// reset backoff if we have a success
		duration = base

		kl.syncLoopMonitor.Store(kl.clock.Now())
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
//   - configCh: dispatch the pods for the config change to the appropriate
//     handler callback for the event type
//   - plegCh: update the runtime cache; sync pod
//   - syncCh: sync all pods waiting for sync
//   - housekeepingCh: trigger cleanup of pods
//   - health manager: sync pods that have failed or in which one or more
//     containers have failed health checks
func (kl *Kubelet) syncLoopIteration(configCh <-chan kubetypes.PodUpdate, handler SyncHandler,
	syncCh <-chan time.Time, housekeepingCh <-chan time.Time, plegCh <-chan *pleg.PodLifecycleEvent) bool {
	select {
	case u, open := <-configCh:
		// Update from a config source; dispatch it to the right handler
		// callback.
		if !open {
			klog.ErrorS(nil, "Update channel is closed, exiting the sync loop")
			return false
		}

		switch u.Op {
		case kubetypes.ADD:
			klog.V(2).InfoS("SyncLoop ADD", "source", u.Source, "pods", klog.KObjs(u.Pods))
			// After restarting, kubelet will get all existing pods through
			// ADD as if they are new pods. These pods will then go through the
			// admission process and *may* be rejected. This can be resolved
			// once we have checkpointing.
			handler.HandlePodAdditions(u.Pods)
		case kubetypes.UPDATE:
			klog.V(2).InfoS("SyncLoop UPDATE", "source", u.Source, "pods", klog.KObjs(u.Pods))
			handler.HandlePodUpdates(u.Pods)
		case kubetypes.REMOVE:
			klog.V(2).InfoS("SyncLoop REMOVE", "source", u.Source, "pods", klog.KObjs(u.Pods))
			// 从kl.podManager里移除这个pod
			//   如果是mirror pod，则从kl.podManager.mirrorPodByUID、kl.podManager.mirrorPodByFullName、kl.podManager.translationByUID删除对应的pod
			//   不是mirror pod，则从kl.podManager.podByUID、kl.podManager.podByFullName中删除这个pod
			// pod是mirror pod
			//   根据mirror pod获取static pod，如果获取成功（有static pod），则重新执行kl.podWorkers.UpdatePod，触发重新执行podWork（event类型为SyncPodUpdate）
			//   否则不做任何事情
			//   其实就是让podWorker执行kl.syncPod或不做任何事情
			// pod不是mirror pod
			//   不是每种source至少有一个Pod（podUpdate事件）,返回错误
			//   重新执行kl.podWorkers.UpdatePod，触发重新执行podWork（event类型为SyncPodKill）,让podWorker执行terminating（static pod文件被移除，生成Remove事件，且podworker里的delete状态值为false）或不做任何事情（podWorker已经finished）
			handler.HandlePodRemoves(u.Pods)
		case kubetypes.RECONCILE:
			klog.V(4).InfoS("SyncLoop RECONCILE", "source", u.Source, "pods", klog.KObjs(u.Pods))
			// pod是普通pod，则添加到podByUID、podByFullName、mirrorPodByFullName
			// pod是mirror pod，则添加到mirrorPodByUID、mirrorPodByFullName、mirrorPodByFullName
			// pod设置了readinessGate，现在pod里有type为"Ready"的condition。且status字段跟要更新的（type为"Ready"的condition）不一样或Message字段不一样
			//   则利用pod worker机制，执行更新pod状态更新（流程跟pod add一样，只是没有记录启动处理时间metrics），主要是为了更新pod status
			// pod的Phase为"Failed"且reason为"Evicted"
			//   获得缓存（kl.podCache.pods）中的id的data里status，如果不在缓存中，则返回空的数据（只有pod id）
			//   找到podStatus里所有container status里的status为"exited"的container status
			//   如果有filterContainerID过滤的container，则还要匹配过滤的container
			//   返回保留最近containersToKeep个后剩余的container status（按照创建时间倒序排列）
			//   发送所有container给kl.containerDeletor.worker通道，让goroutine消费这个通道进行移除这个container
			handler.HandlePodReconcile(u.Pods)
		case kubetypes.DELETE:
			klog.V(2).InfoS("SyncLoop DELETE", "source", u.Source, "pods", klog.KObjs(u.Pods))
			// DELETE is treated as a UPDATE because of graceful deletion.
			// REMOVE和DELETE区别：REMOVE是pod资源已经不存在了，而DELETE是pod的DeletionTimestamp不为nil（pod资源是存在的）
			// 更新kl.podManager内部记录
			//   pod是普通pod，则添加到podByUID、podByFullName、mirrorPodByFullName
			//   pod是mirror pod，则添加到mirrorPodByUID、mirrorPodByFullName、mirrorPodByFullName
			// 如果pod是mirror pod，根据mirror pod获取static pod，如果获取成功（有static pod），则重新执行kl.podWorkers.UpdatePod，触发重新执行podWork（event类型为SyncPodUpdate），否则（static pod已经移除了，mirror pod被housekeeping清理时候，static pod已经在kl.podManager里移除）不做任何事情
			// pod不是mirror pod，根据static pod获取mirror pod（如果不是static pod，mirror pod为nil），重新执行kl.podWorkers.UpdatePod，触发重新执行podWork（event类型为SyncPodUpdate）
			handler.HandlePodUpdates(u.Pods)
		case kubetypes.SET:
			// TODO: Do we want to support this?
			klog.ErrorS(nil, "Kubelet does not support snapshot update")
		default:
			klog.ErrorS(nil, "Invalid operation type received", "operation", u.Op)
		}

		kl.sourcesReady.AddSource(u.Source)

	case e := <-plegCh:
		if e.Type == pleg.ContainerStarted {
			// record the most recent time we observed a container start for this pod.
			// this lets us selectively invalidate the runtimeCache when processing a delete for this pod
			// to make sure we don't miss handling graceful termination for containers we reported as having started.
			kl.lastContainerStartedTime.Add(e.ID, time.Now())
		}
		// 这个event类型，"ContainerStarted"、"ContainerDied"、"ContainerRemoved"的PodLifecycleEvent
		// event类型不是"ContainerRemoved"，则返回true
		if isSyncPodWorthy(e) {
			// PLEG event for a pod; sync it.
			// 根据uid返回非mirror pod（普通pod和static pod），（如果是static pod文件移除（生成Remove事件），则这个是获取不到pod，因为在SyncLoop REMOVE handler.HandlePodRemoves里会从podManager中移除）
			if pod, ok := kl.podManager.GetPodByUID(e.ID); ok {
				klog.V(2).InfoS("SyncLoop (PLEG): event for pod", "pod", klog.KObj(pod), "event", e)
				// 遍历pods，重新执行kl.podWorkers.UpdatePod（type为kubetypes.SyncPodSync），触发重新执行podWorker
				handler.HandlePodSyncs([]*v1.Pod{pod})
			} else {
				// If the pod no longer exists, ignore the event.
				klog.V(4).InfoS("SyncLoop (PLEG): pod does not exist, ignore irrelevant event", "event", e)
			}
		}

		// event类型是"ContainerDied"
		if e.Type == pleg.ContainerDied {
			if containerID, ok := e.Data.(string); ok {
				// 从kl.podCache中成功获得pod uid对应的kubecontainer.PodStatus
				//   如果podID在p.podSyncStatuses里，则当pod是被驱逐或"pod被删除且处于terminated状态"，
				//   或podID不在p.podSyncStatuses里，且"至少一个pod worker执行了syncPod，即已经有pod通过UpdatePod()启动"，则移除所有的退出的container
				//   否则只移除这个container
				// 获取不成功，则不做任何事情
				kl.cleanUpContainersInPod(e.ID, containerID)
			}
		}
	case <-syncCh:
		// Sync pods waiting for sync
		// 从kl.podManager里所有static pod和普通的pod进行筛选，返回的pod包含
		// 1. 这个pod uid是在pod worker中处理过的uid（执行完成的uid，包括已经执行成功和执行失败）
		// 2. pod中还未被pod worker处理过，且kl.podSyncLoopHandler（返回true的）
		// kl.podSyncLoopHandler只有一个pkg\kubelet\active_deadline.go activeDeadlineHandler
		// activeDeadlineHandler
		// 检测pod status中StartTime距现在时间长度，超出pod.Spec.ActiveDeadlineSeconds，返回true
		// 即pod的active时间长度是否超过pod.Spec.ActiveDeadlineSeconds
		podsToSync := kl.getPodsToSync()
		if len(podsToSync) == 0 {
			break
		}
		klog.V(4).InfoS("SyncLoop (SYNC) pods", "total", len(podsToSync), "pods", klog.KObjs(podsToSync))
		// 遍历pods，重新执行kl.podWorkers.UpdatePod（type为kubetypes.SyncPodSync），触发重新执行podWorker
		handler.HandlePodSyncs(podsToSync)
	case update := <-kl.livenessManager.Updates():
		if update.Result == proberesults.Failure {
			handleProbeSync(kl, update, handler, "liveness", "unhealthy")
		}
	case update := <-kl.readinessManager.Updates():
		ready := update.Result == proberesults.Success
		kl.statusManager.SetContainerReadiness(update.PodUID, update.ContainerID, ready)

		status := ""
		if ready {
			status = "ready"
		}
		handleProbeSync(kl, update, handler, "readiness", status)
	case update := <-kl.startupManager.Updates():
		started := update.Result == proberesults.Success
		kl.statusManager.SetContainerStartup(update.PodUID, update.ContainerID, started)

		status := "unhealthy"
		if started {
			status = "started"
		}
		handleProbeSync(kl, update, handler, "startup", status)
	case <-housekeepingCh:
		// 不是每种source至少有一个Pod（podUpdate事件）
		if !kl.sourcesReady.AllReady() {
			// If the sources aren't ready or volume manager has not yet synced the states,
			// skip housekeeping, as we may accidentally delete pods from unready sources.
			klog.V(4).InfoS("SyncLoop (housekeeping, skipped): sources aren't ready yet")
		} else {
			start := time.Now()
			klog.V(4).InfoS("SyncLoop (housekeeping)")
			// 停止并移除不存在的pod的pod worker，并停止并删除周期性执行container的readniess、liveness、start probe的worker
			// 移除不存在pod的容器（容器还在运行，但是不在apiserver中）
			// 移除不存在pod的volume和pod目录
			// 从apiserver中删除所有没有对应static pod的mirror pod
			// 启用cgroupsPerQOS，则处理cgroup路径下不存在的pod
			//   如果pod还存在volume，且不启用keepTerminatedPodVolumes，则pod的cpu cgroup里的cpu share的值为2
			//   否则启动一个goroutine 移除所有cgroup子系统里的cgroup路径
			// 容器重启的回退周期清理，清理开始回退时间离现在已经超过2倍最大回收周期的item
			// 解决pod被删除后又被创建且相同uid问题
			//   如果pod不处于生命周期的最后阶段（phase处于"Succeeded"或"Failed"状态），则让podWorker启动pod
			if err := handler.HandlePodCleanups(); err != nil {
				klog.ErrorS(err, "Failed cleaning pods")
			}
			duration := time.Since(start)
			if duration > housekeepingWarningDuration {
				klog.ErrorS(fmt.Errorf("housekeeping took too long"), "Housekeeping took longer than 15s", "seconds", duration.Seconds())
			}
			klog.V(4).InfoS("SyncLoop (housekeeping) end")
		}
	}
	return true
}

func handleProbeSync(kl *Kubelet, update proberesults.Update, handler SyncHandler, probe, status string) {
	// We should not use the pod from manager, because it is never updated after initialization.
	pod, ok := kl.podManager.GetPodByUID(update.PodUID)
	if !ok {
		// If the pod no longer exists, ignore the update.
		klog.V(4).InfoS("SyncLoop (probe): ignore irrelevant update", "probe", probe, "status", status, "update", update)
		return
	}
	klog.V(1).InfoS("SyncLoop (probe)", "probe", probe, "status", status, "pod", klog.KObj(pod))
	handler.HandlePodSyncs([]*v1.Pod{pod})
}

// dispatchWork starts the asynchronous sync of the pod in a pod worker.
// If the pod has completed termination, dispatchWork will perform no action.
// 重新执行kl.podWorkers.UpdatePod，触发重新执行podWork
func (kl *Kubelet) dispatchWork(pod *v1.Pod, syncType kubetypes.SyncPodType, mirrorPod *v1.Pod, start time.Time) {
	// Run the sync in an async worker.
	kl.podWorkers.UpdatePod(UpdatePodOptions{
		Pod:        pod,
		MirrorPod:  mirrorPod,
		UpdateType: syncType,
		StartTime:  start,
	})
	// Note the number of containers for new pods.
	if syncType == kubetypes.SyncPodCreate {
		metrics.ContainersPerPodCount.Observe(float64(len(pod.Spec.Containers)))
	}
}

// TODO: handle mirror pods in a separate component (issue #17251)
// 根据mirror pod获取static pod，如果获取成功（有static pod），则重新执行kl.podWorkers.UpdatePod，触发重新执行podWork（event类型为SyncPodUpdate）
// 否则不做任何事情
func (kl *Kubelet) handleMirrorPod(mirrorPod *v1.Pod, start time.Time) {
	// Mirror pod ADD/UPDATE/DELETE operations are considered an UPDATE to the
	// corresponding static pod. Send update to the pod worker if the static
	// pod exists.
	// 根据mirror pod获取static pod
	if pod, ok := kl.podManager.GetPodByMirrorPod(mirrorPod); ok {
		// 重新执行kl.podWorkers.UpdatePod，触发重新执行podWork（event类型为SyncPodUpdate）
		kl.dispatchWork(pod, kubetypes.SyncPodUpdate, mirrorPod, start)
	}
}

// HandlePodAdditions is the callback in SyncHandler for pods being added from
// a config source.
func (kl *Kubelet) HandlePodAdditions(pods []*v1.Pod) {
	start := kl.clock.Now()
	sort.Sort(sliceutils.PodsByCreationTime(pods))
	for _, pod := range pods {
		existingPods := kl.podManager.GetPods()
		// Always add the pod to the pod manager. Kubelet relies on the pod
		// manager as the source of truth for the desired state. If a pod does
		// not exist in the pod manager, it means that it has been deleted in
		// the apiserver and no action (other than cleanup) is required.
		kl.podManager.AddPod(pod)

		if kubetypes.IsMirrorPod(pod) {
			kl.handleMirrorPod(pod, start)
			continue
		}

		// Only go through the admission process if the pod is not requested
		// for termination by another part of the kubelet. If the pod is already
		// using resources (previously admitted), the pod worker is going to be
		// shutting it down. If the pod hasn't started yet, we know that when
		// the pod worker is invoked it will also avoid setting up the pod, so
		// we simply avoid doing any work.
		if !kl.podWorkers.IsPodTerminationRequested(pod.UID) {
			// We failed pods that we rejected, so activePods include all admitted
			// pods that are alive.
			activePods := kl.filterOutInactivePods(existingPods)

			// Check if we can admit the pod; if not, reject it.
			if ok, reason, message := kl.canAdmitPod(activePods, pod); !ok {
				kl.rejectPod(pod, reason, message)
				continue
			}
		}
		mirrorPod, _ := kl.podManager.GetMirrorPodByPod(pod)
		kl.dispatchWork(pod, kubetypes.SyncPodCreate, mirrorPod, start)
	}
}

// HandlePodUpdates is the callback in the SyncHandler interface for pods
// being updated from a config source.
// 更新kl.podManager内部记录
//   pod是普通pod，则添加到podByUID、podByFullName、mirrorPodByFullName
//   pod是mirror pod，则添加到mirrorPodByUID、mirrorPodByFullName、mirrorPodByFullName
// 如果pod是mirror pod，根据mirror pod获取static pod，如果获取成功（有static pod），则重新执行kl.podWorkers.UpdatePod，触发重新执行podWork（event类型为SyncPodUpdate），否则（static pod已经移除了，mirror pod被housekeeping清理时候，static pod已经在kl.podManager里移除）不做任何事情
// pod不是mirror pod，根据static pod获取mirror pod（如果不是static pod，mirror pod为nil），重新执行kl.podWorkers.UpdatePod，触发重新执行podWork（event类型为SyncPodUpdate）
func (kl *Kubelet) HandlePodUpdates(pods []*v1.Pod) {
	start := kl.clock.Now()
	for _, pod := range pods {
		// pod是普通pod，则添加到podByUID、podByFullName、mirrorPodByFullName
		// pod是mirror pod，则添加到mirrorPodByUID、mirrorPodByFullName、mirrorPodByFullName
		kl.podManager.UpdatePod(pod)
		// 如果pod是mirror pod
		if kubetypes.IsMirrorPod(pod) {
			// 根据mirror pod获取static pod，如果获取成功（有static pod），则重新执行kl.podWorkers.UpdatePod，触发重新执行podWork（event类型为SyncPodUpdate）
			// 否则不做任何事情
			kl.handleMirrorPod(pod, start)
			continue
		}
		// pod不是mirror pod
		// 根据static pod获取mirror pod（如果不是static pod，返回nil）
		mirrorPod, _ := kl.podManager.GetMirrorPodByPod(pod)
		// 重新执行kl.podWorkers.UpdatePod，触发重新执行podWork（event类型为SyncPodUpdate）
		kl.dispatchWork(pod, kubetypes.SyncPodUpdate, mirrorPod, start)
	}
}

// HandlePodRemoves is the callback in the SyncHandler interface for pods
// being removed from a config source.
// 从kl.podManager里移除这个pod
//   如果是mirror pod，则从kl.podManager.mirrorPodByUID、kl.podManager.mirrorPodByFullName、kl.podManager.translationByUID删除对应的pod
//   不是mirror pod，则从kl.podManager.podByUID、kl.podManager.podByFullName中删除这个pod
// pod是mirror pod
//   根据mirror pod获取static pod，如果获取成功（有static pod），则重新执行kl.podWorkers.UpdatePod，触发重新执行podWork（event类型为SyncPodUpdate）
//   否则不做任何事情
//   其实就是让podWorker执行kl.syncPod或不做任何事情
// pod不是mirror pod
//   不是每种source至少有一个Pod（podUpdate事件）,返回错误
//   重新执行kl.podWorkers.UpdatePod，触发重新执行podWork（event类型为SyncPodKill）,让podWorker执行terminating（static pod文件被移除，生成Remove事件）或不做任何事情（podWorker已经finished）
func (kl *Kubelet) HandlePodRemoves(pods []*v1.Pod) {
	start := kl.clock.Now()
	for _, pod := range pods {
		// 如果是mirror pod，则从kl.podManager.mirrorPodByUID、kl.podManager.mirrorPodByFullName、kl.podManager.translationByUID删除对应的pod
		// 不是mirror pod，则从kl.podManager.podByUID、kl.podManager.podByFullName中删除这个pod
		kl.podManager.DeletePod(pod)
		// pod是mirror pod
		if kubetypes.IsMirrorPod(pod) {
			// 根据mirror pod获取static pod，如果获取成功（有static pod，一般都能获取成功，即使上面从podManager中移除mirror pod（pm.podByFullName不受影响），但是kl.podManager.GetPodByMirrorPod是根据pod的full name来查找pod的（pm.podByFullName中查找）），则重新执行kl.podWorkers.UpdatePod，触发重新执行podWork（event类型为SyncPodUpdate）
			// 否则不做任何事情
			// 其实就是让podWorker执行kl.syncPod或不做任何事情
			kl.handleMirrorPod(pod, start)
			continue
		}
		// Deletion is allowed to fail because the periodic cleanup routine
		// will trigger deletion again.
		// pod不是mirror pod
		// 不是每种source至少有一个Pod（podUpdate事件）,返回错误
		// 重新执行kl.podWorkers.UpdatePod，触发重新执行podWork（event类型为SyncPodKill）,让podWorker执行terminating（static pod文件被移除）或不做任何事情（podWorker已经finished）
		if err := kl.deletePod(pod); err != nil {
			klog.V(2).InfoS("Failed to delete pod", "pod", klog.KObj(pod), "err", err)
		}
	}
}

// HandlePodReconcile is the callback in the SyncHandler interface for pods
// that should be reconciled.
// pod是普通pod，则添加到podByUID、podByFullName、mirrorPodByFullName
// pod是mirror pod，则添加到mirrorPodByUID、mirrorPodByFullName、mirrorPodByFullName
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
		kl.podManager.UpdatePod(pod)

		// Reconcile Pod "Ready" condition if necessary. Trigger sync pod for reconciliation.
		// 现在pod里有type为"Ready"的condition。且status字段跟生成的（type为"Ready"的condition）不一样或Message字段不一样，则返回true
		// 为了处理pod有ReadinessGates情况，没有ReadinessGates则返回false
		if status.NeedToReconcilePodReadiness(pod) {
			mirrorPod, _ := kl.podManager.GetMirrorPodByPod(pod)
			// 重新执行kl.podWorkers.UpdatePod，触发重新执行podWork（event类型为SyncPodSync）， 其实就是让podWorker执行kl.syncPod
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
// 遍历pods，重新执行kl.podWorkers.UpdatePod（type为kubetypes.SyncPodSync），触发重新执行podWork
func (kl *Kubelet) HandlePodSyncs(pods []*v1.Pod) {
	start := kl.clock.Now()
	for _, pod := range pods {
		// 根据static pod获取mirror pod
		mirrorPod, _ := kl.podManager.GetMirrorPodByPod(pod)
		// 重新执行kl.podWorkers.UpdatePod，触发重新执行podWork
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
func (kl *Kubelet) updateRuntimeUp() {
	kl.updateRuntimeMux.Lock()
	defer kl.updateRuntimeMux.Unlock()

	s, err := kl.containerRuntime.Status()
	if err != nil {
		klog.ErrorS(err, "Container runtime sanity check failed")
		return
	}
	if s == nil {
		klog.ErrorS(nil, "Container runtime status is nil")
		return
	}
	// Periodically log the whole runtime status for debugging.
	klog.V(4).InfoS("Container runtime status", "status", s)
	networkReady := s.GetRuntimeCondition(kubecontainer.NetworkReady)
	if networkReady == nil || !networkReady.Status {
		klog.ErrorS(nil, "Container runtime network not ready", "networkReady", networkReady)
		kl.runtimeState.setNetworkState(fmt.Errorf("container runtime network not ready: %v", networkReady))
	} else {
		// Set nil if the container runtime network is ready.
		kl.runtimeState.setNetworkState(nil)
	}
	// information in RuntimeReady condition will be propagated to NodeReady condition.
	runtimeReady := s.GetRuntimeCondition(kubecontainer.RuntimeReady)
	// If RuntimeReady is not set or is false, report an error.
	if runtimeReady == nil || !runtimeReady.Status {
		klog.ErrorS(nil, "Container runtime not ready", "runtimeReady", runtimeReady)
		kl.runtimeState.setRuntimeState(fmt.Errorf("container runtime not ready: %v", runtimeReady))
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
func (kl *Kubelet) ListenAndServe(kubeCfg *kubeletconfiginternal.KubeletConfiguration, tlsOptions *server.TLSOptions,
	auth server.AuthInterface) {
	server.ListenAndServeKubeletServer(kl, kl.resourceAnalyzer, kubeCfg, tlsOptions, auth)
}

// ListenAndServeReadOnly runs the kubelet HTTP server in read-only mode.
func (kl *Kubelet) ListenAndServeReadOnly(address net.IP, port uint) {
	server.ListenAndServeKubeletReadOnlyServer(kl, kl.resourceAnalyzer, address, port)
}

// ListenAndServePodResources runs the kubelet podresources grpc service
func (kl *Kubelet) ListenAndServePodResources() {
	socket, err := util.LocalEndpoint(kl.getPodResourcesDir(), podresources.Socket)
	if err != nil {
		klog.V(2).InfoS("Failed to get local endpoint for PodResources endpoint", "err", err)
		return
	}
	server.ListenAndServePodResources(socket, kl.podManager, kl.containerManager, kl.containerManager, kl.containerManager)
}

// Delete the eligible dead container instances in a pod. Depending on the configuration, the latest dead containers may be kept around.
// 从kl.podCache中获得pod uid对应的kubecontainer.PodStatus
// 如果podID在p.podSyncStatuses里，则当pod是被驱逐或"pod被删除且处于terminated状态"，
// 或podID不在p.podSyncStatuses里，且"至少一个pod worker执行了syncPod，即已经有pod通过UpdatePod()启动"，则移除所有的退出的container
// 否则只移除这个container
func (kl *Kubelet) cleanUpContainersInPod(podID types.UID, exitedContainerID string) {
	// 返回缓存（kl.podCache.pods）中的id的data里status，如果不在缓存中，则返回空的数据（只有pod id）
	if podStatus, err := kl.podCache.Get(podID); err == nil {
		// When an evicted or deleted pod has already synced, all containers can be removed.
		// 如果podID在p.podSyncStatuses里，则当pod是被驱逐或"pod被删除且处于terminated状态"，返回true，否则返回false
		// 否则在"至少一个pod worker执行了syncPod，即已经有pod通过UpdatePod()启动"，返回true，否则返回false
		// 如果static pod移除后，这里removeAll为false，因为调用UpdatePod的event类型为SyncPodKill，podSyncStatus的deleted为false
		removeAll := kl.podWorkers.ShouldPodContentBeRemoved(podID)
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
			klog.ErrorS(err, "Error getting node")
			continue
		}
		if len(node.Spec.PodCIDRs) != 0 {
			podCIDRs := strings.Join(node.Spec.PodCIDRs, ",")
			if _, err := kl.updatePodCIDR(podCIDRs); err != nil {
				klog.ErrorS(err, "Pod CIDR update failed", "CIDR", podCIDRs)
				continue
			}
			kl.updateRuntimeUp()
			kl.syncNodeStatus()
			return
		}
	}
}

// isSyncPodWorthy filters out events that are not worthy of pod syncing
func isSyncPodWorthy(event *pleg.PodLifecycleEvent) bool {
	// ContainerRemoved doesn't affect pod state
	return event.Type != pleg.ContainerRemoved
}

// Gets the streaming server configuration to use with in-process CRI shims.
func getStreamingConfig(kubeCfg *kubeletconfiginternal.KubeletConfiguration, kubeDeps *Dependencies, crOptions *config.ContainerRuntimeOptions) *streaming.Config {
	config := &streaming.Config{
		StreamIdleTimeout:               kubeCfg.StreamingConnectionIdleTimeout.Duration,
		StreamCreationTimeout:           streaming.DefaultConfig.StreamCreationTimeout,
		SupportedRemoteCommandProtocols: streaming.DefaultConfig.SupportedRemoteCommandProtocols,
		SupportedPortForwardProtocols:   streaming.DefaultConfig.SupportedPortForwardProtocols,
	}
	config.Addr = net.JoinHostPort("localhost", "0")
	return config
}
