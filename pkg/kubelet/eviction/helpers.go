/*
Copyright 2016 The Kubernetes Authors.

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

package eviction

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/klog"
	"k8s.io/kubernetes/pkg/api/v1/pod"
	v1resource "k8s.io/kubernetes/pkg/api/v1/resource"
	statsapi "k8s.io/kubernetes/pkg/kubelet/apis/stats/v1alpha1"
	evictionapi "k8s.io/kubernetes/pkg/kubelet/eviction/api"
	kubetypes "k8s.io/kubernetes/pkg/kubelet/types"
	volumeutils "k8s.io/kubernetes/pkg/volume/util"
)

const (
	unsupportedEvictionSignal = "unsupported eviction signal %v"
	// Reason is the reason reported back in status.
	Reason = "Evicted"
	// nodeLowMessageFmt is the message for evictions due to resource pressure.
	nodeLowMessageFmt = "The node was low on resource: %v. "
	// nodeConditionMessageFmt is the message for evictions due to resource pressure.
	nodeConditionMessageFmt = "The node had condition: %v. "
	// containerMessageFmt provides additional information for containers exceeding requests
	containerMessageFmt = "Container %s was using %s, which exceeds its request of %s. "
	// containerEphemeralStorageMessageFmt provides additional information for containers which have exceeded their ES limit
	containerEphemeralStorageMessageFmt = "Container %s exceeded its local ephemeral storage limit %q. "
	// podEphemeralStorageMessageFmt provides additional information for pods which have exceeded their ES limit
	podEphemeralStorageMessageFmt = "Pod ephemeral local storage usage exceeds the total limit of containers %s. "
	// emptyDirMessageFmt provides additional information for empty-dir volumes which have exceeded their size limit
	emptyDirMessageFmt = "Usage of EmptyDir volume %q exceeds the limit %q. "
	// inodes, number. internal to this module, used to account for local disk inode consumption.
	resourceInodes v1.ResourceName = "inodes"
	// resourcePids, number. internal to this module, used to account for local pid consumption.
	resourcePids v1.ResourceName = "pids"
	// OffendingContainersKey is the key in eviction event annotations for the list of container names which exceeded their requests
	OffendingContainersKey = "offending_containers"
	// OffendingContainersUsageKey is the key in eviction event annotations for the list of usage of containers which exceeded their requests
	OffendingContainersUsageKey = "offending_containers_usage"
	// StarvedResourceKey is the key for the starved resource in eviction event annotations
	StarvedResourceKey = "starved_resource"
)

var (
	// signalToNodeCondition maps a signal to the node condition to report if threshold is met.
	signalToNodeCondition map[evictionapi.Signal]v1.NodeConditionType
	// signalToResource maps a Signal to its associated Resource.
	signalToResource map[evictionapi.Signal]v1.ResourceName
)

func init() {
	// map eviction signals to node conditions
	signalToNodeCondition = map[evictionapi.Signal]v1.NodeConditionType{}
	signalToNodeCondition[evictionapi.SignalMemoryAvailable] = v1.NodeMemoryPressure
	signalToNodeCondition[evictionapi.SignalAllocatableMemoryAvailable] = v1.NodeMemoryPressure
	signalToNodeCondition[evictionapi.SignalImageFsAvailable] = v1.NodeDiskPressure
	signalToNodeCondition[evictionapi.SignalNodeFsAvailable] = v1.NodeDiskPressure
	signalToNodeCondition[evictionapi.SignalImageFsInodesFree] = v1.NodeDiskPressure
	signalToNodeCondition[evictionapi.SignalNodeFsInodesFree] = v1.NodeDiskPressure
	signalToNodeCondition[evictionapi.SignalPIDAvailable] = v1.NodePIDPressure

	// map signals to resources (and vice-versa)
	signalToResource = map[evictionapi.Signal]v1.ResourceName{}
	signalToResource[evictionapi.SignalMemoryAvailable] = v1.ResourceMemory
	signalToResource[evictionapi.SignalAllocatableMemoryAvailable] = v1.ResourceMemory
	signalToResource[evictionapi.SignalImageFsAvailable] = v1.ResourceEphemeralStorage
	signalToResource[evictionapi.SignalImageFsInodesFree] = resourceInodes
	signalToResource[evictionapi.SignalNodeFsAvailable] = v1.ResourceEphemeralStorage
	signalToResource[evictionapi.SignalNodeFsInodesFree] = resourceInodes
	signalToResource[evictionapi.SignalPIDAvailable] = resourcePids
}

// validSignal returns true if the signal is supported.
func validSignal(signal evictionapi.Signal) bool {
	_, found := signalToResource[signal]
	return found
}

// getReclaimableThreshold finds the threshold and resource to reclaim
// 返回thresholds中第一个在signalToResource（Signal与对应相关资源）、对应的资源名称和是否在signalToResource中
// 如果没有找到，返回空、空、false
func getReclaimableThreshold(thresholds []evictionapi.Threshold) (evictionapi.Threshold, v1.ResourceName, bool) {
	for _, thresholdToReclaim := range thresholds {
		if resourceToReclaim, ok := signalToResource[thresholdToReclaim.Signal]; ok {
			return thresholdToReclaim, resourceToReclaim, true
		}
		klog.V(3).Infof("eviction manager: threshold %s was crossed, but reclaim is not implemented for this threshold.", thresholdToReclaim.Signal)
	}
	return evictionapi.Threshold{}, "", false
}

// ParseThresholdConfig parses the flags for thresholds.
// hardEviction与softEviction的区别是hardEviction的GracePeriod一定是0，而softEviction不为0
// 这也是softThresholds与hardThresholds和results都用slice的原因
func ParseThresholdConfig(allocatableConfig []string, evictionHard, evictionSoft, evictionSoftGracePeriod, evictionMinimumReclaim map[string]string) ([]evictionapi.Threshold, error) {
	results := []evictionapi.Threshold{}
	hardThresholds, err := parseThresholdStatements(evictionHard)
	if err != nil {
		return nil, err
	}
	results = append(results, hardThresholds...)
	softThresholds, err := parseThresholdStatements(evictionSoft)
	if err != nil {
		return nil, err
	}
	gracePeriods, err := parseGracePeriods(evictionSoftGracePeriod)
	if err != nil {
		return nil, err
	}
	minReclaims, err := parseMinimumReclaims(evictionMinimumReclaim)
	if err != nil {
		return nil, err
	}
	// 确保所有soft eviction配置都有相应的gracePeriods 
	for i := range softThresholds {
		signal := softThresholds[i].Signal
		period, found := gracePeriods[signal]
		if !found {
			return nil, fmt.Errorf("grace period must be specified for the soft eviction threshold %v", signal)
		}
		softThresholds[i].GracePeriod = period
	}
	results = append(results, softThresholds...)
	for i := range results {
		if minReclaim, ok := minReclaims[results[i].Signal]; ok {
			results[i].MinReclaim = &minReclaim
		}
	}
	for _, key := range allocatableConfig {
		// 如果enforceNodeAllocatable里有pods，添加allocatableMemory.available，所有设置与memory.available一样
		if key == kubetypes.NodeAllocatableEnforcementKey {
			results = addAllocatableThresholds(results)
			break
		}
	}
	return results, nil
}

func addAllocatableThresholds(thresholds []evictionapi.Threshold) []evictionapi.Threshold {
	additionalThresholds := []evictionapi.Threshold{}
	for _, threshold := range thresholds {
		// hardEviction里的memory.available，添加allocatableMemory.available，所有设置与memory.available一样
		if threshold.Signal == evictionapi.SignalMemoryAvailable && isHardEvictionThreshold(threshold) {
			// Copy the SignalMemoryAvailable to SignalAllocatableMemoryAvailable
			additionalThresholds = append(additionalThresholds, evictionapi.Threshold{
				Signal:     evictionapi.SignalAllocatableMemoryAvailable,
				Operator:   threshold.Operator,
				Value:      threshold.Value,
				MinReclaim: threshold.MinReclaim,
			})
		}
	}
	return append(thresholds, additionalThresholds...)
}

// parseThresholdStatements parses the input statements into a list of Threshold objects.
func parseThresholdStatements(statements map[string]string) ([]evictionapi.Threshold, error) {
	if len(statements) == 0 {
		return nil, nil
	}
	results := []evictionapi.Threshold{}
	for signal, val := range statements {
		result, err := parseThresholdStatement(evictionapi.Signal(signal), val)
		if err != nil {
			return nil, err
		}
		if result != nil {
			results = append(results, *result)
		}
	}
	return results, nil
}

// parseThresholdStatement parses a threshold statement and returns a threshold,
// or nil if the threshold should be ignored.
func parseThresholdStatement(signal evictionapi.Signal, val string) (*evictionapi.Threshold, error) {
	if !validSignal(signal) {
		return nil, fmt.Errorf(unsupportedEvictionSignal, signal)
	}
	// 目前所有的都是LessThan
	operator := evictionapi.OpForSignal[signal]
	if strings.HasSuffix(val, "%") {
		// ignore 0% and 100%
		if val == "0%" || val == "100%" {
			return nil, nil
		}
		percentage, err := parsePercentage(val)
		if err != nil {
			return nil, err
		}
		if percentage < 0 {
			return nil, fmt.Errorf("eviction percentage threshold %v must be >= 0%%: %s", signal, val)
		}
		if percentage > 100 {
			return nil, fmt.Errorf("eviction percentage threshold %v must be <= 100%%: %s", signal, val)
		}
		return &evictionapi.Threshold{
			Signal:   signal,
			Operator: operator,
			Value: evictionapi.ThresholdValue{
				Percentage: percentage,
			},
		}, nil
	}
	quantity, err := resource.ParseQuantity(val)
	if err != nil {
		return nil, err
	}
	if quantity.Sign() < 0 || quantity.IsZero() {
		return nil, fmt.Errorf("eviction threshold %v must be positive: %s", signal, &quantity)
	}
	return &evictionapi.Threshold{
		Signal:   signal,
		Operator: operator,
		Value: evictionapi.ThresholdValue{
			Quantity: &quantity,
		},
	}, nil
}

// parsePercentage parses a string representing a percentage value
func parsePercentage(input string) (float32, error) {
	value, err := strconv.ParseFloat(strings.TrimRight(input, "%"), 32)
	if err != nil {
		return 0, err
	}
	return float32(value) / 100, nil
}

// parseGracePeriods parses the grace period statements
func parseGracePeriods(statements map[string]string) (map[evictionapi.Signal]time.Duration, error) {
	if len(statements) == 0 {
		return nil, nil
	}
	results := map[evictionapi.Signal]time.Duration{}
	for signal, val := range statements {
		signal := evictionapi.Signal(signal)
		if !validSignal(signal) {
			return nil, fmt.Errorf(unsupportedEvictionSignal, signal)
		}
		gracePeriod, err := time.ParseDuration(val)
		if err != nil {
			return nil, err
		}
		if gracePeriod < 0 {
			return nil, fmt.Errorf("invalid eviction grace period specified: %v, must be a positive value", val)
		}
		results[signal] = gracePeriod
	}
	return results, nil
}

// parseMinimumReclaims parses the minimum reclaim statements
func parseMinimumReclaims(statements map[string]string) (map[evictionapi.Signal]evictionapi.ThresholdValue, error) {
	if len(statements) == 0 {
		return nil, nil
	}
	results := map[evictionapi.Signal]evictionapi.ThresholdValue{}
	for signal, val := range statements {
		signal := evictionapi.Signal(signal)
		if !validSignal(signal) {
			return nil, fmt.Errorf(unsupportedEvictionSignal, signal)
		}
		if strings.HasSuffix(val, "%") {
			percentage, err := parsePercentage(val)
			if err != nil {
				return nil, err
			}
			if percentage <= 0 {
				return nil, fmt.Errorf("eviction percentage minimum reclaim %v must be positive: %s", signal, val)
			}
			results[signal] = evictionapi.ThresholdValue{
				Percentage: percentage,
			}
			continue
		}
		quantity, err := resource.ParseQuantity(val)
		if err != nil {
			return nil, err
		}
		if quantity.Sign() < 0 {
			return nil, fmt.Errorf("negative eviction minimum reclaim specified for %v", signal)
		}
		results[signal] = evictionapi.ThresholdValue{
			Quantity: &quantity,
		}
	}
	return results, nil
}

// diskUsage converts used bytes into a resource quantity.
// 将fsStats.UsedBytes转城resource.Quantity
func diskUsage(fsStats *statsapi.FsStats) *resource.Quantity {
	if fsStats == nil || fsStats.UsedBytes == nil {
		return &resource.Quantity{Format: resource.BinarySI}
	}
	usage := int64(*fsStats.UsedBytes)
	return resource.NewQuantity(usage, resource.BinarySI)
}

// inodeUsage converts inodes consumed into a resource quantity.
// 将fsStats.InodesUsed转城resource.Quantity
func inodeUsage(fsStats *statsapi.FsStats) *resource.Quantity {
	if fsStats == nil || fsStats.InodesUsed == nil {
		return &resource.Quantity{Format: resource.DecimalSI}
	}
	usage := int64(*fsStats.InodesUsed)
	return resource.NewQuantity(usage, resource.DecimalSI)
}

// memoryUsage converts working set into a resource quantity.
// 将memStats.WorkingSetBytes（statsapi.MemoryStats.WorkingSetBytes）转成resource.Quantity
func memoryUsage(memStats *statsapi.MemoryStats) *resource.Quantity {
	if memStats == nil || memStats.WorkingSetBytes == nil {
		return &resource.Quantity{Format: resource.BinarySI}
	}
	usage := int64(*memStats.WorkingSetBytes)
	return resource.NewQuantity(usage, resource.BinarySI)
}

// localVolumeNames returns the set of volumes for the pod that are local
// TODO: summary API should report what volumes consume local storage rather than hard-code here.
// 返回HostPath、不为"Memory"的EmptyDir、configMap、gitrepo挂载的volume在pod.Spec.Volumes名字列表
func localVolumeNames(pod *v1.Pod) []string {
	result := []string{}
	for _, volume := range pod.Spec.Volumes {
		if volume.HostPath != nil ||
			(volume.EmptyDir != nil && volume.EmptyDir.Medium != v1.StorageMediumMemory) ||
			volume.ConfigMap != nil ||
			volume.GitRepo != nil {
			result = append(result, volume.Name)
		}
	}
	return result
}

// containerUsage aggregates container disk usage and inode consumption for the specified stats to measure.
// 所有container的rootfs和logs的总磁盘使用量和inodes使用量，即包含了"ephemeral-storage"和"inodes"资源的使用量
func containerUsage(podStats statsapi.PodStats, statsToMeasure []fsStatsType) v1.ResourceList {
	disk := resource.Quantity{Format: resource.BinarySI}
	inodes := resource.Quantity{Format: resource.DecimalSI}
	for _, container := range podStats.Containers {
		// "root"在statsToMeasure里
		if hasFsStatsType(statsToMeasure, fsStatsRoot) {
			// *diskUsage(container.Rootfs)为，将container.Rootfs.UsedBytes转城resource.Quantity
			disk.Add(*diskUsage(container.Rootfs))
			// inodeUsage(container.Rootfs)为，将container.Rootfs.InodesUsed转城resource.Quantity
			inodes.Add(*inodeUsage(container.Rootfs))
		}
		// "logs"在statsToMeasure里
		if hasFsStatsType(statsToMeasure, fsStatsLogs) {
			disk.Add(*diskUsage(container.Logs))
			inodes.Add(*inodeUsage(container.Logs))
		}
	}
	// 所有container的rootfs和logs的总磁盘使用量和inodes使用量
	return v1.ResourceList{
		v1.ResourceEphemeralStorage: disk,
		resourceInodes:              inodes,
	}
}

// podLocalVolumeUsage aggregates pod local volumes disk usage and inode consumption for the specified stats to measure.
// 返回所有volumeNames的总磁盘使用量和inode使用量，即"ephemeral-storage"和"inodes"资源使用量
func podLocalVolumeUsage(volumeNames []string, podStats statsapi.PodStats) v1.ResourceList {
	disk := resource.Quantity{Format: resource.BinarySI}
	inodes := resource.Quantity{Format: resource.DecimalSI}
	for _, volumeName := range volumeNames {
		for _, volumeStats := range podStats.VolumeStats {
			if volumeStats.Name == volumeName {
				disk.Add(*diskUsage(&volumeStats.FsStats))
				inodes.Add(*inodeUsage(&volumeStats.FsStats))
				break
			}
		}
	}
	return v1.ResourceList{
		v1.ResourceEphemeralStorage: disk,
		resourceInodes:              inodes,
	}
}

// podDiskUsage aggregates pod disk usage and inode consumption for the specified stats to measure.
// 所有container的（rootfs或logs或rootfs加logs）的总磁盘使用量和inodes使用量，或加上volume（HostPath、不为"Memory"的EmptyDir、configMap、gitrepo挂载）的总磁盘使用量和inodes使用量，即包含了"ephemeral-storage"和"inodes"资源的使用量
func podDiskUsage(podStats statsapi.PodStats, pod *v1.Pod, statsToMeasure []fsStatsType) (v1.ResourceList, error) {
	disk := resource.Quantity{Format: resource.BinarySI}
	inodes := resource.Quantity{Format: resource.DecimalSI}

	// 所有container的（rootfs或logs或rootfs加logs）的总磁盘使用量和inodes使用量，即包含了"ephemeral-storage"和"inodes"资源的使用量
	containerUsageList := containerUsage(podStats, statsToMeasure)
	disk.Add(containerUsageList[v1.ResourceEphemeralStorage])
	inodes.Add(containerUsageList[resourceInodes])

	// "localVolumeSource"在statsToMeasure里，则添加HostPath、不为"Memory"的EmptyDir、configMap、gitrepo挂载的volume的磁盘使用量和inode使用量
	if hasFsStatsType(statsToMeasure, fsStatsLocalVolumeSource) {
		// 返回HostPath、不为"Memory"的EmptyDir、configMap、gitrepo挂载的volume在pod.Spec.Volumes名字列表
		volumeNames := localVolumeNames(pod)
		// 返回所有volumeNames的总磁盘使用量和inode使用量，即"ephemeral-storage"和"inodes"资源使用量
		podLocalVolumeUsageList := podLocalVolumeUsage(volumeNames, podStats)
		disk.Add(podLocalVolumeUsageList[v1.ResourceEphemeralStorage])
		inodes.Add(podLocalVolumeUsageList[resourceInodes])
	}
	return v1.ResourceList{
		v1.ResourceEphemeralStorage: disk,
		resourceInodes:              inodes,
	}, nil
}

// localEphemeralVolumeNames returns the set of ephemeral volumes for the pod that are local
// pod里所有GitRepo、不为"Memory"的EmptyDir、configMap、DownwardAPI类型的volume在pod.Spec.Volumes名字集合
func localEphemeralVolumeNames(pod *v1.Pod) []string {
	result := []string{}
	for _, volume := range pod.Spec.Volumes {
		// volume是GitRepo、不为"Memory"的EmptyDir、configMap、DownwardAPI，返回true
		if volumeutils.IsLocalEphemeralVolume(volume) {
			result = append(result, volume.Name)
		}
	}
	return result
}

// podLocalEphemeralStorageUsage aggregates pod local ephemeral storage usage and inode consumption for the specified stats to measure.
// 所有container的（rootfs或logs或rootfs加logs）的总磁盘使用量和inodes使用量，或加上volume（GitRepo、不为"Memory"的EmptyDir、configMap、DownwardAPI）的总磁盘使用量和inodes使用量，或加上pod的etcHost文件大小和inode使用里（为1），即包含了"ephemeral-storage"和"inodes"资源的使用量
func podLocalEphemeralStorageUsage(podStats statsapi.PodStats, pod *v1.Pod, statsToMeasure []fsStatsType, etcHostsPath string) (v1.ResourceList, error) {
	disk := resource.Quantity{Format: resource.BinarySI}
	inodes := resource.Quantity{Format: resource.DecimalSI}

	// 所有container的rootfs和logs的总磁盘使用量和inodes使用量，即包含了"ephemeral-storage"和"inodes"资源的使用量
	containerUsageList := containerUsage(podStats, statsToMeasure)
	disk.Add(containerUsageList[v1.ResourceEphemeralStorage])
	inodes.Add(containerUsageList[resourceInodes])

	// "localVolumeSource"在statsToMeasure里
	if hasFsStatsType(statsToMeasure, fsStatsLocalVolumeSource) {
		// pod里所有GitRepo、不为"Memory"的EmptyDir、configMap、DownwardAPI类型的volume在pod.Spec.Volumes名字集合
		volumeNames := localEphemeralVolumeNames(pod)
		// 返回所有volumeNames的总磁盘使用量和inode使用量（"ephemeral-storage"和"inodes"资源使用量）
		podLocalVolumeUsageList := podLocalVolumeUsage(volumeNames, podStats)
		disk.Add(podLocalVolumeUsageList[v1.ResourceEphemeralStorage])
		inodes.Add(podLocalVolumeUsageList[resourceInodes])
	}
	if len(etcHostsPath) > 0 {
		if stat, err := os.Stat(etcHostsPath); err == nil {
			disk.Add(*resource.NewQuantity(int64(stat.Size()), resource.BinarySI))
			inodes.Add(*resource.NewQuantity(int64(1), resource.DecimalSI))
		}
	}
	return v1.ResourceList{
		v1.ResourceEphemeralStorage: disk,
		resourceInodes:              inodes,
	}, nil
}

// formatThreshold formats a threshold for logging.
func formatThreshold(threshold evictionapi.Threshold) string {
	return fmt.Sprintf("threshold(signal=%v, operator=%v, value=%v, gracePeriod=%v)", threshold.Signal, threshold.Operator, evictionapi.ThresholdValue(threshold.Value), threshold.GracePeriod)
}

// cachedStatsFunc returns a statsFunc based on the provided pod stats.
// 返回通过pod查找statsapi.PodStats的函数
func cachedStatsFunc(podStats []statsapi.PodStats) statsFunc {
	uid2PodStats := map[string]statsapi.PodStats{}
	for i := range podStats {
		uid2PodStats[podStats[i].PodRef.UID] = podStats[i]
	}
	return func(pod *v1.Pod) (statsapi.PodStats, bool) {
		stats, found := uid2PodStats[string(pod.UID)]
		return stats, found
	}
}

// Cmp compares p1 and p2 and returns:
//
//   -1 if p1 <  p2
//    0 if p1 == p2
//   +1 if p1 >  p2
//
type cmpFunc func(p1, p2 *v1.Pod) int

// multiSorter implements the Sort interface, sorting changes within.
type multiSorter struct {
	pods []*v1.Pod
	cmp  []cmpFunc
}

// Sort sorts the argument slice according to the less functions passed to OrderedBy.
func (ms *multiSorter) Sort(pods []*v1.Pod) {
	ms.pods = pods
	sort.Sort(ms)
}

// OrderedBy returns a Sorter that sorts using the cmp functions, in order.
// Call its Sort method to sort the data.
func orderedBy(cmp ...cmpFunc) *multiSorter {
	return &multiSorter{
		cmp: cmp,
	}
}

// Len is part of sort.Interface.
func (ms *multiSorter) Len() int {
	return len(ms.pods)
}

// Swap is part of sort.Interface.
func (ms *multiSorter) Swap(i, j int) {
	ms.pods[i], ms.pods[j] = ms.pods[j], ms.pods[i]
}

// Less is part of sort.Interface.
func (ms *multiSorter) Less(i, j int) bool {
	p1, p2 := ms.pods[i], ms.pods[j]
	var k int
	for k = 0; k < len(ms.cmp)-1; k++ {
		cmpResult := ms.cmp[k](p1, p2)
		// p1 is less than p2
		if cmpResult < 0 {
			return true
		}
		// p1 is greater than p2
		if cmpResult > 0 {
			return false
		}
		// we don't know yet
	}
	// the last cmp func is the final decider
	return ms.cmp[k](p1, p2) < 0
}

// priority compares pods by Priority, if priority is enabled.
// 比较pod的优先级
// p1和p2优先级相等，则返回0
// p1大于p2优先级，则返回1
// p1小于p2优先级，则返回-1
func priority(p1, p2 *v1.Pod) int {
	priority1 := pod.GetPodPriority(p1)
	priority2 := pod.GetPodPriority(p2)
	if priority1 == priority2 {
		return 0
	}
	if priority1 > priority2 {
		return 1
	}
	return -1
}

// exceedMemoryRequests compares whether or not pods' memory usage exceeds their requests
// 返回比较函数
// 先比较是否都有stat，如果没有stat，则返回比较是否有stat函数
// 都有stat，则返回比较"memory"资源的使用量，是否大于所有container的"memory"资源的request总的大小函数
func exceedMemoryRequests(stats statsFunc) cmpFunc {
	return func(p1, p2 *v1.Pod) int {
		p1Stats, p1Found := stats(p1)
		p2Stats, p2Found := stats(p2)
		// 出现至少一个没有stat
		if !p1Found || !p2Found {
			// prioritize evicting the pod for which no stats were found
			// p1Found等于p2Found，都为false，返回0
			// p1Found不等于p2Found，且p1Found为false，p2Found为true，返回-1
			// p1Found不等于p2Found，且p1Found为true，p2Found为false，返回1
			return cmpBool(!p1Found, !p2Found)
		}

		// 将p1Stats.Memory.WorkingSetBytes转成resource.Quantity
		p1Memory := memoryUsage(p1Stats.Memory)
		p2Memory := memoryUsage(p2Stats.Memory)
		// v1resource.GetResourceRequestQuantity为
		// 所有container的"memory"资源的request总的大小
		// 如果所有container的"memory"资源的总request小于所有init container里的request的最大值，则使用init container的request
		// pod定义了Overhead且启用了"PodOverhead"，"memory"资源的request总的大小（pod里container的资源request）不为0，则"memory"资源的request总的大小加上podOverhead
		// "memory"资源的使用量，是否大于所有container的diskResource资源的request总的大小
		p1ExceedsRequests := p1Memory.Cmp(v1resource.GetResourceRequestQuantity(p1, v1.ResourceMemory)) == 1
		p2ExceedsRequests := p2Memory.Cmp(v1resource.GetResourceRequestQuantity(p2, v1.ResourceMemory)) == 1
		// prioritize evicting the pod which exceeds its requests
		// p1ExceedsRequests等于p2ExceedsRequests，返回0
		// p1ExceedsRequests为true，p2ExceedsRequests为false，返回-1
		// p1ExceedsRequests为false，p2ExceedsRequests为true，返回1
		return cmpBool(p1ExceedsRequests, p2ExceedsRequests)
	}
}

// memory compares pods by largest consumer of memory relative to request.
// 返回比较函数
// 先比较是否都有stat，如果没有stat，则返回比较是否有stat函数
// 都有stat，则返回比较"memory"资源的使用量，超出所有container的"memory"资源的总request数量函数
func memory(stats statsFunc) cmpFunc {
	return func(p1, p2 *v1.Pod) int {
		p1Stats, p1Found := stats(p1)
		p2Stats, p2Found := stats(p2)
		// 出现至少一个没有stat
		if !p1Found || !p2Found {
			// prioritize evicting the pod for which no stats were found
			// p1Found等于p2Found，都为false，返回0
			// p1Found不等于p2Found，且p1Found为false，p2Found为true，返回-1
			// p1Found不等于p2Found，且p1Found为true，p2Found为false，返回1
			return cmpBool(!p1Found, !p2Found)
		}

		// adjust p1, p2 usage relative to the request (if any)
		// 将p1Stats.Memory.WorkingSetBytes转成resource.Quantity
		p1Memory := memoryUsage(p1Stats.Memory)
		// v1resource.GetResourceRequestQuantity为
		// 所有container的"memory"资源的request总的大小
		// 如果所有container的"memory"资源的总request小于所有init container里的request的最大值，则使用init container的request
		// pod定义了Overhead且启用了"PodOverhead"，"memory"资源的request总的大小（pod里container的资源request）不为0，则"memory"资源的request总的大小加上podOverhead
		p1Request := v1resource.GetResourceRequestQuantity(p1, v1.ResourceMemory)
		// "memory"资源的使用量超出，所有container的"memory"资源的总request的数量
		p1Memory.Sub(p1Request)

		p2Memory := memoryUsage(p2Stats.Memory)
		p2Request := v1resource.GetResourceRequestQuantity(p2, v1.ResourceMemory)
		p2Memory.Sub(p2Request)

		// prioritize evicting the pod which has the larger consumption of memory
		// p1Memory超出的数量等于p2Memory超出的数量，返回0
		// p1Memory超出的数量小于p2Memory超出的数量，返回1
		// p1Memory超出的数量大于p2Memory超出的数量，返回-1
		return p2Memory.Cmp(*p1Memory)
	}
}

// exceedDiskRequests compares whether or not pods' disk usage exceeds their requests
// 返回比较函数
// 先比较是否都有stat，如果没有stat，则返回比较是否有stat函数
// 都有stat，则返回比较diskResource资源的使用量，是否大于所有container的diskResource资源的request总的大小函数
// diskResource可以是"ephemeral-storage"和"inodes"
func exceedDiskRequests(stats statsFunc, fsStatsToMeasure []fsStatsType, diskResource v1.ResourceName) cmpFunc {
	return func(p1, p2 *v1.Pod) int {
		p1Stats, p1Found := stats(p1)
		p2Stats, p2Found := stats(p2)
		// 出现至少一个未找到
		if !p1Found || !p2Found {
			// prioritize evicting the pod for which no stats were found
			// p1Found等于p2Found，都为false，返回0
			// p1Found不等于p2Found，且p1Found为false，p2Found为true，返回-1
			// p1Found不等于p2Found，且p1Found为true，p2Found为false，返回1
			return cmpBool(!p1Found, !p2Found)
		}

		// 所有container的（rootfs或logs或rootfs加logs）的总磁盘使用量和inodes使用量，或加上volume（HostPath、不为"Memory"的EmptyDir、configMap、gitrepo挂载）的总磁盘使用量和inodes使用量，即包含了"ephemeral-storage"和"inodes"资源的使用量
		p1Usage, p1Err := podDiskUsage(p1Stats, p1, fsStatsToMeasure)
		p2Usage, p2Err := podDiskUsage(p2Stats, p2, fsStatsToMeasure)
		// podDiskUsage返回的err都为nil
		if p1Err != nil || p2Err != nil {
			// prioritize evicting the pod which had an error getting stats
			return cmpBool(p1Err != nil, p2Err != nil)
		}

		p1Disk := p1Usage[diskResource]
		p2Disk := p2Usage[diskResource]
		// v1resource.GetResourceRequestQuantity为
		// 所有container的diskResource资源的request总的大小
		// 如果所有container的diskResource资源的总request小于所有init container里的request的最大值，则使用init container的request
		// pod定义了Overhead且启用了"PodOverhead"，diskResource资源的request总的大小（pod里container的资源request）不为0，则diskResource资源的request总的大小加上podOverhead
		// diskResource资源的使用量，是否大于所有container的diskResource资源的request总的大小
		p1ExceedsRequests := p1Disk.Cmp(v1resource.GetResourceRequestQuantity(p1, diskResource)) == 1
		p2ExceedsRequests := p2Disk.Cmp(v1resource.GetResourceRequestQuantity(p2, diskResource)) == 1
		// prioritize evicting the pod which exceeds its requests
		// p1ExceedsRequests等于p2ExceedsRequests，返回0
		// p1ExceedsRequests为true，p2ExceedsRequests为false，返回-1
		// p1ExceedsRequests为false，p2ExceedsRequests为true，返回1
		return cmpBool(p1ExceedsRequests, p2ExceedsRequests)
	}
}

// disk compares pods by largest consumer of disk relative to request for the specified disk resource.
// 返回比较函数
// 先比较是否都有stat，如果没有stat，则返回比较是否有stat函数
// 都有stat，则返回比较diskResource资源的使用量，超出所有container的diskResource资源的总request数量函数
// diskResource可以是"ephemeral-storage"和"inodes"
func disk(stats statsFunc, fsStatsToMeasure []fsStatsType, diskResource v1.ResourceName) cmpFunc {
	return func(p1, p2 *v1.Pod) int {
		p1Stats, p1Found := stats(p1)
		p2Stats, p2Found := stats(p2)
		if !p1Found || !p2Found {
			// prioritize evicting the pod for which no stats were found
			// p1Found等于p2Found，都为false，返回0
			// p1Found不等于p2Found，且p1Found为false，p2Found为true，返回-1
			// p1Found不等于p2Found，且p1Found为true，p2Found为false，返回1
			return cmpBool(!p1Found, !p2Found)
		}
		// 所有container的（rootfs或logs或rootfs加logs）的总磁盘使用量和inodes使用量，或加上volume（HostPath、不为"Memory"的EmptyDir、configMap、gitrepo挂载）的总磁盘使用量和inodes使用量，即包含了"ephemeral-storage"和"inodes"资源的使用量
		p1Usage, p1Err := podDiskUsage(p1Stats, p1, fsStatsToMeasure)
		p2Usage, p2Err := podDiskUsage(p2Stats, p2, fsStatsToMeasure)
		// podDiskUsage返回的err都为nil
		if p1Err != nil || p2Err != nil {
			// prioritize evicting the pod which had an error getting stats
			return cmpBool(p1Err != nil, p2Err != nil)
		}

		// adjust p1, p2 usage relative to the request (if any)
		p1Disk := p1Usage[diskResource]
		p2Disk := p2Usage[diskResource]
		// 所有container的diskResource资源的request总的大小
		// 如果所有container的diskResource资源的总request小于所有init container里的request的最大值，则使用init container的request
		// pod定义了Overhead且启用了"PodOverhead"，diskResource资源的request总的大小（pod里container的资源request）不为0，则diskResource资源的request总的大小加上podOverhead
		p1Request := v1resource.GetResourceRequestQuantity(p1, v1.ResourceEphemeralStorage)
		// diskResource资源的使用量超出，所有container的diskResource资源的总request的数量
		p1Disk.Sub(p1Request)
		p2Request := v1resource.GetResourceRequestQuantity(p2, v1.ResourceEphemeralStorage)
		p2Disk.Sub(p2Request)
		// prioritize evicting the pod which has the larger consumption of disk
		// p1Disk超出的数量等于p2Disk超出的数量，返回0
		// p1Disk超出的数量小于p2Disk超出的数量，返回1
		// p1Disk超出的数量大于p2Disk超出的数量，返回-1
		return p2Disk.Cmp(p1Disk)
	}
}

// cmpBool compares booleans, placing true before false
// a等于b返回0
// a不等于b，且b为false，返回-1
// a不等于b，且a为false，返回1
func cmpBool(a, b bool) int {
	if a == b {
		return 0
	}
	if !b {
		return -1
	}
	return 1
}

// rankMemoryPressure orders the input pods for eviction in response to memory pressure.
// It ranks by whether or not the pod's usage exceeds its requests, then by priority, and
// finally by memory usage above requests.
// 先进行比较"memory"资源的使用量，是否大于所有container的"memory"资源的request总的大小函数。结果不相同，则未超出的排后面
// 如果相同，再进行优先级比较。不相同，则优先级大的排在后面
// 如果优先级相同，则比较超出request资源的使用量（有可能是负数）。超出数量不一样，超出少的排在后面。超出数量一样，按照出现顺序进行排序
func rankMemoryPressure(pods []*v1.Pod, stats statsFunc) {
	// exceedMemoryRequests
	// 先比较是否都有stat，如果没有stat，则返回比较是否有stat函数
	// 都有stat，则返回比较"memory"资源的使用量，是否大于所有container的"memory"资源的request总的大小函数
	// priority
	// 比较pod的优先级
	// p1和p2优先级相等，则返回0
	// p1大于p2优先级，则返回1
	// p1小于p2优先级，则返回-1
	// memory
	// 先比较是否都有stat，如果没有stat，则返回比较是否有stat函数
	// 都有stat，则返回比较"memory"资源的使用量，超出所有container的"memory"资源的总request数量函数
	orderedBy(exceedMemoryRequests(stats), priority, memory(stats)).Sort(pods)
}

// rankPIDPressure orders the input pods by priority in response to PID pressure.
// 比较pod的优先级
// p1和p2优先级相等，p1排在p2后面
// p1大于p2优先级，p1排在p2后面
// p1小于p2优先级，p1排在p2前面
func rankPIDPressure(pods []*v1.Pod, stats statsFunc) {
	orderedBy(priority).Sort(pods)
}

// rankDiskPressureFunc returns a rankFunc that measures the specified fs stats.
// 先进行比较diskResource资源的使用量，是否大于所有container的diskResource资源的request总的大小函数。结果不相同，则未超出的排后面
// 如果相同，再进行优先级比较。不相同，则优先级大的排在后面
// 如果优先级相同，则比较超出request资源的使用量（有可能是负数）。超出数量不一样，超出少的排在后面。超出数量一样，进行按照出现顺序进行排序
func rankDiskPressureFunc(fsStatsToMeasure []fsStatsType, diskResource v1.ResourceName) rankFunc {
	return func(pods []*v1.Pod, stats statsFunc) {
		// exceedDiskRequests
		// 先比较是否都有stat，如果没有stat，则返回比较是否有stat函数
		// 都有stat，则返回比较diskResource资源的使用量，是否大于所有container的diskResource资源的request总的大小函数
		// priority
		// 比较pod的优先级
		// p1和p2优先级相等，则返回0
		// p1大于p2优先级，则返回1
		// p1小于p2优先级，则返回-1
		// disk
		// 返回比较函数
		// 先比较是否都有stat，如果没有stat，则返回比较是否有stat函数
		// 都有stat，则返回比较diskResource资源的使用量，超出所有container的diskResource资源的总request数量函数
		orderedBy(exceedDiskRequests(stats, fsStatsToMeasure, diskResource), priority, disk(stats, fsStatsToMeasure, diskResource)).Sort(pods)
	}
}

// byEvictionPriority implements sort.Interface for []v1.ResourceName.
type byEvictionPriority []evictionapi.Threshold

func (a byEvictionPriority) Len() int      { return len(a) }
func (a byEvictionPriority) Swap(i, j int) { a[i], a[j] = a[j], a[i] }

// Less ranks memory before all other resources, and ranks thresholds with no resource to reclaim last
// 驱逐类型为"memory.available"和"allocatableMemory.available"排在前面，没有资源类型排在最后面
func (a byEvictionPriority) Less(i, j int) bool {
	_, jSignalHasResource := signalToResource[a[j].Signal]
	return a[i].Signal == evictionapi.SignalMemoryAvailable || a[i].Signal == evictionapi.SignalAllocatableMemoryAvailable || !jSignalHasResource
}

// makeSignalObservations derives observations using the specified summary provider.
// 利用summary.Pods生成通过pod查找statsapi.PodStats的函数
// 从summary.Node获得各个阈值类型的available和capacity和time
func makeSignalObservations(summary *statsapi.Summary) (signalObservations, statsFunc) {
	// build the function to work against for pod stats
	// 返回通过pod查找statsapi.PodStats的函数
	statsFunc := cachedStatsFunc(summary.Pods)
	// build an evaluation context for current eviction signals
	result := signalObservations{}

	// 有summary.Node.memory.AvailableBytes和memory.WorkingSetBytes监控数据，则设置阈值类型"memory.available"的signalObservation
	if memory := summary.Node.Memory; memory != nil && memory.AvailableBytes != nil && memory.WorkingSetBytes != nil {
		// 阈值类型是"memory.available"
		result[evictionapi.SignalMemoryAvailable] = signalObservation{
			available: resource.NewQuantity(int64(*memory.AvailableBytes), resource.BinarySI),
			capacity:  resource.NewQuantity(int64(*memory.AvailableBytes+*memory.WorkingSetBytes), resource.BinarySI),
			time:      memory.Time,
		}
	}
	// 从summary.Node.SystemContainers获得name为"pods"的statsapi.ContainerStats（默认为"/kubepods.slice"容器的监控数据，cgroup driver是systemd）
	if allocatableContainer, err := getSysContainer(summary.Node.SystemContainers, statsapi.SystemContainerPods); err != nil {
		klog.Errorf("eviction manager: failed to construct signal: %q error: %v", evictionapi.SignalAllocatableMemoryAvailable, err)
	} else {
		// "/kubepods.slice"容器的监控数据有memory.AvailableBytes和memory.WorkingSetBytes监控数据，则设置阈值类型"allocatableMemory.available"的signalObservation
		if memory := allocatableContainer.Memory; memory != nil && memory.AvailableBytes != nil && memory.WorkingSetBytes != nil {
			result[evictionapi.SignalAllocatableMemoryAvailable] = signalObservation{
				available: resource.NewQuantity(int64(*memory.AvailableBytes), resource.BinarySI),
				capacity:  resource.NewQuantity(int64(*memory.AvailableBytes+*memory.WorkingSetBytes), resource.BinarySI),
				time:      memory.Time,
			}
		}
	}
	// 有summary.Node.Fs.AvailableBytes和summary.Node.Fs.CapacityBytes监控数据，则设置阈值类型为"nodefs.available"
	// 有summary.Node.Fs.InodesFree和summary.Node.Fs.Inodes监控数据，则设置阈值类型为"nodefs.inodesFree"
	if nodeFs := summary.Node.Fs; nodeFs != nil {
		if nodeFs.AvailableBytes != nil && nodeFs.CapacityBytes != nil {
			result[evictionapi.SignalNodeFsAvailable] = signalObservation{
				available: resource.NewQuantity(int64(*nodeFs.AvailableBytes), resource.BinarySI),
				capacity:  resource.NewQuantity(int64(*nodeFs.CapacityBytes), resource.BinarySI),
				time:      nodeFs.Time,
			}
		}
		if nodeFs.InodesFree != nil && nodeFs.Inodes != nil {
			result[evictionapi.SignalNodeFsInodesFree] = signalObservation{
				available: resource.NewQuantity(int64(*nodeFs.InodesFree), resource.DecimalSI),
				capacity:  resource.NewQuantity(int64(*nodeFs.Inodes), resource.DecimalSI),
				time:      nodeFs.Time,
			}
		}
	}
	// 有summary.Node.Runtime.ImageFs.AvailableBytes和summary.Node.Runtime.ImageFs.CapacityBytes监控数据，则设置阈值类型"imagefs.available"的signalObservation
	// 有summary.Node.Runtime.ImageFs.InodesFree和summary.Node.Runtime.ImageFs.Inodes监控数据，则设置阈值类型"imagefs.inodesFree"的signalObservation
	if summary.Node.Runtime != nil {
		if imageFs := summary.Node.Runtime.ImageFs; imageFs != nil {
			if imageFs.AvailableBytes != nil && imageFs.CapacityBytes != nil {
				result[evictionapi.SignalImageFsAvailable] = signalObservation{
					available: resource.NewQuantity(int64(*imageFs.AvailableBytes), resource.BinarySI),
					capacity:  resource.NewQuantity(int64(*imageFs.CapacityBytes), resource.BinarySI),
					time:      imageFs.Time,
				}
				if imageFs.InodesFree != nil && imageFs.Inodes != nil {
					result[evictionapi.SignalImageFsInodesFree] = signalObservation{
						available: resource.NewQuantity(int64(*imageFs.InodesFree), resource.DecimalSI),
						capacity:  resource.NewQuantity(int64(*imageFs.Inodes), resource.DecimalSI),
						time:      imageFs.Time,
					}
				}
			}
		}
	}
	// 有summary.Node.Rlimit.NumOfRunningProcesses和summary.Node.Rlimit.MaxPID的监控数据，则设置阈值类型"pid.available"的signalObservation
	if rlimit := summary.Node.Rlimit; rlimit != nil {
		if rlimit.NumOfRunningProcesses != nil && rlimit.MaxPID != nil {
			available := int64(*rlimit.MaxPID) - int64(*rlimit.NumOfRunningProcesses)
			result[evictionapi.SignalPIDAvailable] = signalObservation{
				available: resource.NewQuantity(available, resource.BinarySI),
				capacity:  resource.NewQuantity(int64(*rlimit.MaxPID), resource.BinarySI),
				time:      rlimit.Time,
			}
		}
	}
	return result, statsFunc
}

// 从sysContainers里找到statsapi.ContainerStats.name为name
func getSysContainer(sysContainers []statsapi.ContainerStats, name string) (*statsapi.ContainerStats, error) {
	for _, cont := range sysContainers {
		if cont.Name == name {
			return &cont, nil
		}
	}
	return nil, fmt.Errorf("system container %q not found in metrics", name)
}

// thresholdsMet returns the set of thresholds that were met independent of grace period
// 返回所有需要保留资源值大于可用资源值（到达evict阈值）的类型的阈值设置
// 如果enforceMinReclaim为true（启用最小回收）且阈值类型里定义了MinReclaim，则保留资源需要加上MinReclaim
func thresholdsMet(thresholds []evictionapi.Threshold, observations signalObservations, enforceMinReclaim bool) []evictionapi.Threshold {
	results := []evictionapi.Threshold{}
	for i := range thresholds {
		threshold := thresholds[i]
		// 找到阈值类型的监控值（available值和capacity值和time）
		observed, found := observations[threshold.Signal]
		if !found {
			klog.Warningf("eviction manager: no observation found for eviction signal %v", threshold.Signal)
			continue
		}
		// determine if we have met the specified threshold
		thresholdMet := false
		// 计算百分比之后的值或原始值（直接返回保留多少资源的值）
		quantity := evictionapi.GetThresholdQuantity(threshold.Value, observed.capacity)
		// if enforceMinReclaim is specified, we compare relative to value - minreclaim
		// 如果enforceMinReclaim为true（启用最小回收）且阈值类型里定义了MinReclaim，则保留资源需要加上MinReclaim
		if enforceMinReclaim && threshold.MinReclaim != nil {
			quantity.Add(*evictionapi.GetThresholdQuantity(*threshold.MinReclaim, observed.capacity))
		}
		thresholdResult := quantity.Cmp(*observed.available)
		switch threshold.Operator {
		case evictionapi.OpLessThan:
			// 需要保留资源值是否大于可用资源值
			thresholdMet = thresholdResult > 0
		}
		// 需要保留资源值大于可用资源值
		if thresholdMet {
			results = append(results, threshold)
		}
	}
	return results
}

func debugLogObservations(logPrefix string, observations signalObservations) {
	if !klog.V(3) {
		return
	}
	for k, v := range observations {
		if !v.time.IsZero() {
			klog.Infof("eviction manager: %v: signal=%v, available: %v, capacity: %v, time: %v", logPrefix, k, v.available, v.capacity, v.time)
		} else {
			klog.Infof("eviction manager: %v: signal=%v, available: %v, capacity: %v", logPrefix, k, v.available, v.capacity)
		}
	}
}

func debugLogThresholdsWithObservation(logPrefix string, thresholds []evictionapi.Threshold, observations signalObservations) {
	if !klog.V(3) {
		return
	}
	for i := range thresholds {
		threshold := thresholds[i]
		observed, found := observations[threshold.Signal]
		if found {
			quantity := evictionapi.GetThresholdQuantity(threshold.Value, observed.capacity)
			klog.Infof("eviction manager: %v: threshold [signal=%v, quantity=%v] observed %v", logPrefix, threshold.Signal, quantity, observed.available)
		} else {
			klog.Infof("eviction manager: %v: threshold [signal=%v] had no observation", logPrefix, threshold.Signal)
		}
	}
}

// thresholds里所有现在有监控数据，且监控数据时间或数据发生变化的驱逐类型
func thresholdsUpdatedStats(thresholds []evictionapi.Threshold, observations, lastObservations signalObservations) []evictionapi.Threshold {
	results := []evictionapi.Threshold{}
	for i := range thresholds {
		threshold := thresholds[i]
		observed, found := observations[threshold.Signal]
		if !found {
			klog.Warningf("eviction manager: no observation found for eviction signal %v", threshold.Signal)
			continue
		}
		last, found := lastObservations[threshold.Signal]
		// 在上一次监控数据里没有发现，或现在监控数据的time时间为0，或现在监控数据的time时间在上一次监控数据的后面
		if !found || observed.time.IsZero() || observed.time.After(last.time.Time) {
			results = append(results, threshold)
		}
	}
	return results
}

// thresholdsFirstObservedAt merges the input set of thresholds with the previous observation to determine when active set of thresholds were initially met.
// thresholds有threshold不在lastObservedAt里，则添加到result且值为now。如果在lastObservedAt里，则添加到result且值为lastObservedAt里的值，并返回result
func thresholdsFirstObservedAt(thresholds []evictionapi.Threshold, lastObservedAt thresholdsObservedAt, now time.Time) thresholdsObservedAt {
	results := thresholdsObservedAt{}
	for i := range thresholds {
		observedAt, found := lastObservedAt[thresholds[i]]
		if !found {
			observedAt = now
		}
		results[thresholds[i]] = observedAt
	}
	return results
}

// thresholdsMetGracePeriod returns the set of thresholds that have satisfied associated grace period
// 所有达到阈值的类型发生时间超过阈值的GracePeriod，的阈值类型
func thresholdsMetGracePeriod(observedAt thresholdsObservedAt, now time.Time) []evictionapi.Threshold {
	results := []evictionapi.Threshold{}
	for threshold, at := range observedAt {
		duration := now.Sub(at)
		if duration < threshold.GracePeriod {
			klog.V(2).Infof("eviction manager: eviction criteria not yet met for %v, duration: %v", formatThreshold(threshold), duration)
			continue
		}
		results = append(results, threshold)
	}
	return results
}

// nodeConditions returns the set of node conditions associated with a threshold
// 获得thresholds里signal（阈值类型）对应的nodeCondition
func nodeConditions(thresholds []evictionapi.Threshold) []v1.NodeConditionType {
	results := []v1.NodeConditionType{}
	for _, threshold := range thresholds {
		if nodeCondition, found := signalToNodeCondition[threshold.Signal]; found {
			if !hasNodeCondition(results, nodeCondition) {
				results = append(results, nodeCondition)
			}
		}
	}
	return results
}

// nodeConditionsLastObservedAt merges the input with the previous observation to determine when a condition was most recently met.
// 将nodeConditions添加到results里且value为now， lastObservedAt里的condition不在results里，则添加到result里，返回result
// 即将nodeConditions和lastObservedAt进行聚合，并且以nodeConditions里为准
func nodeConditionsLastObservedAt(nodeConditions []v1.NodeConditionType, lastObservedAt nodeConditionsObservedAt, now time.Time) nodeConditionsObservedAt {
	results := nodeConditionsObservedAt{}
	// the input conditions were observed "now"
	for i := range nodeConditions {
		results[nodeConditions[i]] = now
	}
	// the conditions that were not observed now are merged in with their old time
	for key, value := range lastObservedAt {
		_, found := results[key]
		if !found {
			results[key] = value
		}
	}
	return results
}

// nodeConditionsObservedSince returns the set of conditions that have been observed within the specified period
// 返回nodeCondition发生未超过period的condition列表
func nodeConditionsObservedSince(observedAt nodeConditionsObservedAt, period time.Duration, now time.Time) []v1.NodeConditionType {
	results := []v1.NodeConditionType{}
	for nodeCondition, at := range observedAt {
		duration := now.Sub(at)
		if duration < period {
			results = append(results, nodeCondition)
		}
	}
	return results
}

// hasFsStatsType returns true if the fsStat is in the input list
// item是否在inputs里
func hasFsStatsType(inputs []fsStatsType, item fsStatsType) bool {
	for _, input := range inputs {
		if input == item {
			return true
		}
	}
	return false
}

// hasNodeCondition returns true if the node condition is in the input list
// item是否在inputs里
func hasNodeCondition(inputs []v1.NodeConditionType, item v1.NodeConditionType) bool {
	for _, input := range inputs {
		if input == item {
			return true
		}
	}
	return false
}

// mergeThresholds will merge both threshold lists eliminating duplicates.
// 将inputsA和inputsB进行聚合，去除重复的
func mergeThresholds(inputsA []evictionapi.Threshold, inputsB []evictionapi.Threshold) []evictionapi.Threshold {
	results := inputsA
	for _, threshold := range inputsB {
		if !hasThreshold(results, threshold) {
			results = append(results, threshold)
		}
	}
	return results
}

// hasThreshold returns true if the threshold is in the input list
// item在inputs里条件是GracePeriod、Operator、Signal、Value都一样
func hasThreshold(inputs []evictionapi.Threshold, item evictionapi.Threshold) bool {
	for _, input := range inputs {
		if input.GracePeriod == item.GracePeriod && input.Operator == item.Operator && input.Signal == item.Signal && compareThresholdValue(input.Value, item.Value) {
			return true
		}
	}
	return false
}

// compareThresholdValue returns true if the two thresholdValue objects are logically the same
func compareThresholdValue(a evictionapi.ThresholdValue, b evictionapi.ThresholdValue) bool {
	if a.Quantity != nil {
		if b.Quantity == nil {
			return false
		}
		return a.Quantity.Cmp(*b.Quantity) == 0
	}
	if b.Quantity != nil {
		return false
	}
	return a.Percentage == b.Percentage
}

// isHardEvictionThreshold returns true if eviction should immediately occur
// hardEviction与softEviction的区别是hardEviction的GracePeriod一定是0，而softEviction不为0
func isHardEvictionThreshold(threshold evictionapi.Threshold) bool {
	return threshold.GracePeriod == time.Duration(0)
}

// 阈值类型是不是"allocatableMemory.available"
func isAllocatableEvictionThreshold(threshold evictionapi.Threshold) bool {
	return threshold.Signal == evictionapi.SignalAllocatableMemoryAvailable
}

// buildSignalToRankFunc returns ranking functions associated with resources
func buildSignalToRankFunc(withImageFs bool) map[evictionapi.Signal]rankFunc {
	signalToRankFunc := map[evictionapi.Signal]rankFunc{
		// rankMemoryPressure
		// 先进行比较"memory"资源的使用量，是否大于所有container的"memory"资源的request总的大小函数。结果不相同，则未超出的排后面
		// 如果相同，再进行优先级比较。不相同，则优先级大的排在后面
		// 如果优先级相同，则比较超出request资源的使用量（有可能是负数）。超出数量不一样，超出少的排在后面。超出数量一样，进行按照出现顺序进行排序
		// "memory.available"
		// 比较的是memory监控里workset
		evictionapi.SignalMemoryAvailable:            rankMemoryPressure,
		// "allocatableMemory.available"
		// 比较的是memory监控里workset
		evictionapi.SignalAllocatableMemoryAvailable: rankMemoryPressure,
		// "pid.available"
		// 比较pod的优先级
		// p1和p2优先级相等，进行反序（p1排在p2后面）
		// p1大于p2优先级，p1排在p2后面
		// p1小于p2优先级，p1排在p2前面
		evictionapi.SignalPIDAvailable:               rankPIDPressure,
	}
	// usage of an imagefs is optional
	// 镜像保存目录挂载设备与kubelet root目录的挂载设备不一样
	if withImageFs {
		// with an imagefs, nodefs pod rank func for eviction only includes logs and local volumes
		// rankDiskPressureFunc
		// 先进行比较diskResource资源的使用量，是否大于所有container的diskResource资源的request总的大小函数。结果不相同，则未超出的排后面
		// 如果相同，再进行优先级比较。不相同，则优先级大的排在后面
		// 如果优先级相同，则比较超出request资源的使用量（有可能是负数）。超出数量不一样，超出少的排在后面。超出数量一样，进行按照出现顺序进行排序
		// "nodefs.available"是针对"logs"（日志）和"localVolumeSource"（HostPath、不为"Memory"的EmptyDir、configMap、gitrepo挂载的volume）磁盘使用量
		// diskResource资源为"ephemeral-storage"
		signalToRankFunc[evictionapi.SignalNodeFsAvailable] = rankDiskPressureFunc([]fsStatsType{fsStatsLogs, fsStatsLocalVolumeSource}, v1.ResourceEphemeralStorage)
		// "nodefs.inodesFree"是针对"logs"（日志）和"localVolumeSource"（HostPath、不为"Memory"的EmptyDir、configMap、gitrepo挂载的volume）的inode使用量
		// diskResource资源为"inodes"
		signalToRankFunc[evictionapi.SignalNodeFsInodesFree] = rankDiskPressureFunc([]fsStatsType{fsStatsLogs, fsStatsLocalVolumeSource}, resourceInodes)
		// with an imagefs, imagefs pod rank func for eviction only includes rootfs
		// "imagefs.available"是针对"root"的磁盘使用量
		// diskResource资源为"ephemeral-storage"
		signalToRankFunc[evictionapi.SignalImageFsAvailable] = rankDiskPressureFunc([]fsStatsType{fsStatsRoot}, v1.ResourceEphemeralStorage)
		// "imagefs.inodesFree"是针对"root"的inode使用量
		// diskResource资源为"inodes"
		signalToRankFunc[evictionapi.SignalImageFsInodesFree] = rankDiskPressureFunc([]fsStatsType{fsStatsRoot}, resourceInodes)
	} else {
		// without an imagefs, nodefs pod rank func for eviction looks at all fs stats.
		// since imagefs and nodefs share a common device, they share common ranking functions.
		// 镜像保存目录挂载设备与kubelet root目录的挂载设备一样
		// "nodefs.available"是针对"root"和"logs"（日志）和"localVolumeSource"（HostPath、不为"Memory"的EmptyDir、configMap、gitrepo挂载的volume）磁盘使用量
		// diskResource资源为"ephemeral-storage"
		signalToRankFunc[evictionapi.SignalNodeFsAvailable] = rankDiskPressureFunc([]fsStatsType{fsStatsRoot, fsStatsLogs, fsStatsLocalVolumeSource}, v1.ResourceEphemeralStorage)
		// "nodefs.inodesFree"是针对"root"和"logs"（日志）和"localVolumeSource"（HostPath、不为"Memory"的EmptyDir、configMap、gitrepo挂载的volume）的inode使用量
		// diskResource资源为"inodes"
		signalToRankFunc[evictionapi.SignalNodeFsInodesFree] = rankDiskPressureFunc([]fsStatsType{fsStatsRoot, fsStatsLogs, fsStatsLocalVolumeSource}, resourceInodes)
		// "imagefs.available"针对"root"和"logs"（日志）和"localVolumeSource"（HostPath、不为"Memory"的EmptyDir、configMap、gitrepo挂载的volume）的磁盘使用量
		// diskResource资源为"ephemeral-storage"
		signalToRankFunc[evictionapi.SignalImageFsAvailable] = rankDiskPressureFunc([]fsStatsType{fsStatsRoot, fsStatsLogs, fsStatsLocalVolumeSource}, v1.ResourceEphemeralStorage)
		// "imagefs.inodesFree"是针对"root"和"logs"（日志）和"localVolumeSource"（HostPath、不为"Memory"的EmptyDir、configMap、gitrepo挂载的volume）的inode使用量
		// diskResource资源为"inodes"
		signalToRankFunc[evictionapi.SignalImageFsInodesFree] = rankDiskPressureFunc([]fsStatsType{fsStatsRoot, fsStatsLogs, fsStatsLocalVolumeSource}, resourceInodes)
	}
	return signalToRankFunc
}

// PodIsEvicted returns true if the reported pod status is due to an eviction.
// pod的Phase为"Failed"且reason为"Evicted"，返回true
func PodIsEvicted(podStatus v1.PodStatus) bool {
	return podStatus.Phase == v1.PodFailed && podStatus.Reason == Reason
}

// buildSignalToNodeReclaimFuncs returns reclaim functions associated with resources.
func buildSignalToNodeReclaimFuncs(imageGC ImageGC, containerGC ContainerGC, withImageFs bool) map[evictionapi.Signal]nodeReclaimFuncs {
	signalToReclaimFunc := map[evictionapi.Signal]nodeReclaimFuncs{}
	// usage of an imagefs is optional
	// 镜像保存目录挂载设备与kubelet root目录的挂载设备不一样
	if withImageFs {
		// with an imagefs, nodefs pressure should just delete logs
		// "nodefs.available"没有回收函数
		signalToReclaimFunc[evictionapi.SignalNodeFsAvailable] = nodeReclaimFuncs{}
		// "nodefs.inodesFree"没有回收函数
		signalToReclaimFunc[evictionapi.SignalNodeFsInodesFree] = nodeReclaimFuncs{}
		// with an imagefs, imagefs pressure should delete unused images
		// "imagefs.available"
		// imageGC.DeleteUnusedImages 实现在pkg\kubelet\images\image_gc_manager.go
		// 移除node上未使用的镜像
		// containerGC.DeleteAllUnusedContainers 实现在pkg\kubelet\container\container_gc.go
		// 移除可以移除的container
		// 获取非running container且container createdAt时间距离现在已经经过minAge的container
		// allSourcesReady为true，则移除所有可以驱逐的container
		// 根据gcPolicy.MaxPerPodContainer，移除每个pod里老的可以移除的container
		// 根据gcPolicy.MaxContainers，移除pod里或所有container里老的可以移除的container
		//
		// 移除可以移除的sandbox
		// 如果pod删除了或pod处于Terminated状态，则移除所有不为running且没有container属于它的sandbox容器
		// 如果pod没有被删除了，且pod不处于Terminated状态，则保留pod的最近一个sanbox容器（移除其他老的且不为running且没有container属于它的pod）
		//
		// 移除pod日志目录
		// allSourcesReady为true，且pod（pod uid从目录名中提取）不为删除状态，则递归移除目录"/var/log/pods/{pod namespace}_{pod name}_{pod uid}"
		// "/var/log/containers/*.log"文件，如果链接的目标文件不存在，则移除这个链接
		signalToReclaimFunc[evictionapi.SignalImageFsAvailable] = nodeReclaimFuncs{containerGC.DeleteAllUnusedContainers, imageGC.DeleteUnusedImages}
		signalToReclaimFunc[evictionapi.SignalImageFsInodesFree] = nodeReclaimFuncs{containerGC.DeleteAllUnusedContainers, imageGC.DeleteUnusedImages}
	} else {
		// without an imagefs, nodefs pressure should delete logs, and unused images
		// since imagefs and nodefs share a common device, they share common reclaim functions
		signalToReclaimFunc[evictionapi.SignalNodeFsAvailable] = nodeReclaimFuncs{containerGC.DeleteAllUnusedContainers, imageGC.DeleteUnusedImages}
		signalToReclaimFunc[evictionapi.SignalNodeFsInodesFree] = nodeReclaimFuncs{containerGC.DeleteAllUnusedContainers, imageGC.DeleteUnusedImages}
		signalToReclaimFunc[evictionapi.SignalImageFsAvailable] = nodeReclaimFuncs{containerGC.DeleteAllUnusedContainers, imageGC.DeleteUnusedImages}
		signalToReclaimFunc[evictionapi.SignalImageFsInodesFree] = nodeReclaimFuncs{containerGC.DeleteAllUnusedContainers, imageGC.DeleteUnusedImages}
	}
	return signalToReclaimFunc
}

// evictionMessage constructs a useful message about why an eviction occurred, and annotations to provide metadata about the eviction
// 返回"The node was low on resource: {resourceToReclaim}. Container {container name} was using {usage}, which exceeds its request of {request}. "
// annotations["offending_containers"]为所有使用量大于request的container
// annotations["offending_containers_usage"]为所有使用量大于request的container的资源使用量
// annotations["starved_resource"]为资源类型
func evictionMessage(resourceToReclaim v1.ResourceName, pod *v1.Pod, stats statsFunc) (message string, annotations map[string]string) {
	annotations = make(map[string]string)
	// "The node was low on resource: {resourceToReclaim}. "
	message = fmt.Sprintf(nodeLowMessageFmt, resourceToReclaim)
	containers := []string{}
	containerUsage := []string{}
	podStats, ok := stats(pod)
	if !ok {
		return
	}
	for _, containerStats := range podStats.Containers {
		for _, container := range pod.Spec.Containers {
			if container.Name == containerStats.Name {
				// container里的request设置
				requests := container.Resources.Requests[resourceToReclaim]
				var usage *resource.Quantity
				switch resourceToReclaim {
				// "ephemeral-storage"
				case v1.ResourceEphemeralStorage:
					if containerStats.Rootfs != nil && containerStats.Rootfs.UsedBytes != nil && containerStats.Logs != nil && containerStats.Logs.UsedBytes != nil {
						// rootfs的使用量加上logs使用量
						usage = resource.NewQuantity(int64(*containerStats.Rootfs.UsedBytes+*containerStats.Logs.UsedBytes), resource.BinarySI)
					}
				// "memory"
				case v1.ResourceMemory:
					if containerStats.Memory != nil && containerStats.Memory.WorkingSetBytes != nil {
						// 使用量为WorkingSetBytes
						usage = resource.NewQuantity(int64(*containerStats.Memory.WorkingSetBytes), resource.BinarySI)
					}
				}
				// 使用量大于request
				if usage != nil && usage.Cmp(requests) > 0 {
					// "Container {container name} was using {usage}, which exceeds its request of {request}. "
					message += fmt.Sprintf(containerMessageFmt, container.Name, usage.String(), requests.String())
					containers = append(containers, container.Name)
					containerUsage = append(containerUsage, usage.String())
				}
			}
		}
	}
	// annotations["offending_containers"]为所有使用量大于request的container
	annotations[OffendingContainersKey] = strings.Join(containers, ",")
	// annotations["offending_containers_usage"]为所有使用量大于request的container的资源使用量
	annotations[OffendingContainersUsageKey] = strings.Join(containerUsage, ",")
	// annotations["starved_resource"]为资源类型
	annotations[StarvedResourceKey] = string(resourceToReclaim)
	return
}
