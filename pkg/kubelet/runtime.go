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
	"errors"
	"fmt"
	"sync"
	"time"

	utilerrors "k8s.io/apimachinery/pkg/util/errors"
)

type runtimeState struct {
	sync.RWMutex
	lastBaseRuntimeSync      time.Time
	baseRuntimeSyncThreshold time.Duration
	networkError             error
	runtimeError             error
	storageError             error
	cidr                     string
	healthChecks             []*healthCheck
}

// A health check function should be efficient and not rely on external
// components (e.g., container runtime).
type healthCheckFnType func() (bool, error)

type healthCheck struct {
	name string
	fn   healthCheckFnType
}

func (s *runtimeState) addHealthCheck(name string, f healthCheckFnType) {
	s.Lock()
	defer s.Unlock()
	s.healthChecks = append(s.healthChecks, &healthCheck{name: name, fn: f})
}

// 在kubelet.updateRuntimeUp(在pkg\kubelet\kubelet.go)里调用s.setRuntimeSync
func (s *runtimeState) setRuntimeSync(t time.Time) {
	s.Lock()
	defer s.Unlock()
	s.lastBaseRuntimeSync = t
}

// 在kubelet.updateRuntimeUp(在pkg\kubelet\kubelet.go)里
// 调用kubelet.runtimeService.Status()（通过cri api查询）查询runtime Status--包括RuntimeReady、NetworkReady
// NetworkReady Status为false或NetworkReady为nil，会调用s.setNetworkState
func (s *runtimeState) setNetworkState(err error) {
	s.Lock()
	defer s.Unlock()
	s.networkError = err
}

// 在kubelet.updateRuntimeUp(在pkg\kubelet\kubelet.go)里
// 调用kubelet.runtimeService.Status()（通过cri api查询）查询runtime Status--包括RuntimeReady、NetworkReady
// RuntimeReady Status为false或RuntimeReady为nil，会调用s.setRuntimeState
func (s *runtimeState) setRuntimeState(err error) {
	s.Lock()
	defer s.Unlock()
	s.runtimeError = err
}

// 在pkg\kubelet\volume_host.go里的kubeletVolumeHost.SetKubeletError里调用s.setStorageState
func (s *runtimeState) setStorageState(err error) {
	s.Lock()
	defer s.Unlock()
	s.storageError = err
}

func (s *runtimeState) setPodCIDR(cidr string) {
	s.Lock()
	defer s.Unlock()
	s.cidr = cidr
}

func (s *runtimeState) podCIDR() string {
	s.RLock()
	defer s.RUnlock()
	return s.cidr
}

func (s *runtimeState) runtimeErrors() error {
	s.RLock()
	defer s.RUnlock()
	errs := []error{}
	// 在kubelet.updateRuntimeUp(在pkg\kubelet\kubelet.go)里调用s.setRuntimeSync设置s.lastBaseRuntimeSync
	// 如果s.lastBaseRuntimeSync.IsZero()为true，说明kubelet.updateRuntimeUp未执行或未执行完
	if s.lastBaseRuntimeSync.IsZero() {
		errs = append(errs, errors.New("container runtime status check may not have completed yet"))
	} else if !s.lastBaseRuntimeSync.Add(s.baseRuntimeSyncThreshold).After(time.Now()) {
		errs = append(errs, errors.New("container runtime is down"))
	}
	// healthChecks目前只有pelg Healthy()（在pkg\kubelet\pleg\generic.go）
	// 检测PELG.relistTime是否3分钟更新过
	for _, hc := range s.healthChecks {
		if ok, err := hc.fn(); !ok {
			errs = append(errs, fmt.Errorf("%s is not healthy: %v", hc.name, err))
		}
	}
	// 在kubelet.updateRuntimeUp(在pkg\kubelet\kubelet.go)里调用s.setRuntimeState
	// 调用kubelet.runtimeService.Status()（通过cri api查询）查询runtime Status--包括RuntimeReady、NetworkReady
	// 如果RuntimeReady Status为false，则s.runtimeError不为nil
	if s.runtimeError != nil {
		errs = append(errs, s.runtimeError)
	}

	return utilerrors.NewAggregate(errs)
}

// 在kubelet.updateRuntimeUp(在pkg\kubelet\kubelet.go)里调用s.setNetworkState设置s.networkError
// 调用kubelet.runtimeService.Status()（通过cri api查询）查询runtime Status--包括RuntimeReady、NetworkReady
// 如果NetworkReady Status为false，则s.networkError不为nil
func (s *runtimeState) networkErrors() error {
	s.RLock()
	defer s.RUnlock()
	errs := []error{}
	if s.networkError != nil {
		errs = append(errs, s.networkError)
	}
	return utilerrors.NewAggregate(errs)
}

// csi插件是未初始化完成（在pkg\volume\csi\csi_plugin.go里的initializeCSINode调用(kvh *kubeletVolumeHost) SetKubeletError），则s.storageError不为nil
// 而pkg\kubelet\volume_host.go里的kubeletVolumeHost.SetKubeletError里调用s.setStorageState
func (s *runtimeState) storageErrors() error {
	s.RLock()
	defer s.RUnlock()
	errs := []error{}
	if s.storageError != nil {
		errs = append(errs, s.storageError)
	}
	return utilerrors.NewAggregate(errs)
}

func newRuntimeState(
	runtimeSyncThreshold time.Duration,
) *runtimeState {
	return &runtimeState{
		lastBaseRuntimeSync:      time.Time{},
		baseRuntimeSyncThreshold: runtimeSyncThreshold,
		networkError:             ErrNetworkUnknown,
	}
}
