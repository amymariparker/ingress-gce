/*
Copyright 2017 The Kubernetes Authors.

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

package neg

import (
	"fmt"
	"sync"

	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	negsyncer "k8s.io/ingress-gce/pkg/neg/syncers"
	negtypes "k8s.io/ingress-gce/pkg/neg/types"
	"k8s.io/klog"
)

type serviceKey struct {
	namespace string
	name      string
}

// syncerManager contains all the active syncer goroutines and manage their lifecycle.
type syncerManager struct {
	negSyncerType NegSyncerType

	namer      negtypes.NetworkEndpointGroupNamer
	recorder   record.EventRecorder
	cloud      negtypes.NetworkEndpointGroupCloud
	zoneGetter negtypes.ZoneGetter

	serviceLister  cache.Indexer
	endpointLister cache.Indexer

	// TODO: lock per service instead of global lock
	mu sync.Mutex
	// svcPortMap is the canonical indicator for whether a service needs NEG.
	// key consists of service namespace and name. Value is a map of ServicePort
	// Port:TargetPort, which represents ports that require NEG
	svcPortMap map[serviceKey]negtypes.PortInfoMap
	// syncerMap stores the NEG syncer
	// key consists of service namespace, name and targetPort. Value is the corresponding syncer.
	syncerMap map[negsyncer.NegSyncerKey]negtypes.NegSyncer
}

func newSyncerManager(namer negtypes.NetworkEndpointGroupNamer, recorder record.EventRecorder, cloud negtypes.NetworkEndpointGroupCloud, zoneGetter negtypes.ZoneGetter, serviceLister cache.Indexer, endpointLister cache.Indexer, negSyncerType NegSyncerType) *syncerManager {
	klog.V(2).Infof("NEG controller will use NEG syncer type: %q", negSyncerType)
	return &syncerManager{
		negSyncerType:  negSyncerType,
		namer:          namer,
		recorder:       recorder,
		cloud:          cloud,
		zoneGetter:     zoneGetter,
		serviceLister:  serviceLister,
		endpointLister: endpointLister,
		svcPortMap:     make(map[serviceKey]negtypes.PortInfoMap),
		syncerMap:      make(map[negsyncer.NegSyncerKey]negtypes.NegSyncer),
	}
}

// EnsureSyncer starts and stops syncers based on the input service ports.
func (manager *syncerManager) EnsureSyncers(namespace, name string, newPorts negtypes.PortInfoMap) error {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	key := getServiceKey(namespace, name)
	currentPorts, ok := manager.svcPortMap[key]
	if !ok {
		currentPorts = make(negtypes.PortInfoMap)
	}

	removes := currentPorts.Difference(newPorts)
	adds := newPorts.Difference(currentPorts)

	manager.svcPortMap[key] = newPorts
	klog.V(3).Infof("EnsureSyncer %v/%v: syncing %v ports, removing %v ports, adding %v ports", namespace, name, newPorts, removes, adds)

	for svcPort, portInfo := range removes {
		syncer, ok := manager.syncerMap[getSyncerKey(namespace, name, svcPort, portInfo.TargetPort)]
		if ok {
			syncer.Stop()
		}
	}

	errList := []error{}
	// Ensure a syncer is running for each port that is being added.
	for svcPort, portInfo := range adds {
		syncer, ok := manager.syncerMap[getSyncerKey(namespace, name, svcPort, portInfo.TargetPort)]
		if !ok {
			syncerKey := negsyncer.NegSyncerKey{
				Namespace:  namespace,
				Name:       name,
				Port:       svcPort,
				TargetPort: portInfo.TargetPort,
			}

			if manager.negSyncerType == transactionSyncer {
				syncer = negsyncer.NewTransactionSyncer(
					syncerKey,
					portInfo.NegName,
					manager.recorder,
					manager.cloud,
					manager.zoneGetter,
					manager.serviceLister,
					manager.endpointLister,
				)
			} else {
				// Use batch syncer by default
				syncer = negsyncer.NewBatchSyncer(
					syncerKey,
					portInfo.NegName,
					manager.recorder,
					manager.cloud,
					manager.zoneGetter,
					manager.serviceLister,
					manager.endpointLister,
				)
			}

			manager.syncerMap[getSyncerKey(namespace, name, svcPort, portInfo.TargetPort)] = syncer
		}

		if syncer.IsStopped() {
			if err := syncer.Start(); err != nil {
				errList = append(errList, err)
			}
		}
	}

	return utilerrors.NewAggregate(errList)
}

// StopSyncer stops all syncers for the input service.
func (manager *syncerManager) StopSyncer(namespace, name string) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	key := getServiceKey(namespace, name)
	if ports, ok := manager.svcPortMap[key]; ok {
		for svcPort, portInfo := range ports {
			if syncer, ok := manager.syncerMap[getSyncerKey(namespace, name, svcPort, portInfo.TargetPort)]; ok {
				syncer.Stop()
			}
		}
		delete(manager.svcPortMap, key)
	}
	return
}

// Sync signals all syncers related to the service to sync.
func (manager *syncerManager) Sync(namespace, name string) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	key := getServiceKey(namespace, name)
	if portInfoMap, ok := manager.svcPortMap[key]; ok {
		for svcPort, portInfo := range portInfoMap {
			if syncer, ok := manager.syncerMap[getSyncerKey(namespace, name, svcPort, portInfo.TargetPort)]; ok {
				if !syncer.IsStopped() {
					syncer.Sync()
				}
			}
		}
	}
}

// ShutDown signals all syncers to stop
func (manager *syncerManager) ShutDown() {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	for _, s := range manager.syncerMap {
		s.Stop()
	}
}

// GC garbage collects syncers and NEGs.
func (manager *syncerManager) GC() error {
	klog.V(2).Infof("Start NEG garbage collection.")
	defer klog.V(2).Infof("NEG garbage collection finished.")
	// Garbage collect Syncers
	manager.garbageCollectSyncer()

	// Garbage collect NEGs
	if err := manager.garbageCollectNEG(); err != nil {
		return fmt.Errorf("failed to garbage collect negs: %v", err)
	}
	return nil
}

// garbageCollectSyncer removes stopped syncer from syncerMap
func (manager *syncerManager) garbageCollectSyncer() {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	for key, syncer := range manager.syncerMap {
		if syncer.IsStopped() && !syncer.IsShuttingDown() {
			delete(manager.syncerMap, key)
		}
	}
}

func (manager *syncerManager) garbageCollectNEG() error {
	// Retrieve aggregated NEG list from cloud
	// Compare against svcPortMap and Remove unintended NEGs by best effort
	zoneNEGList, err := manager.cloud.AggregatedListNetworkEndpointGroup()
	if err != nil {
		return fmt.Errorf("failed to retrieve aggregated NEG list: %v", err)
	}

	negNames := sets.String{}
	for _, list := range zoneNEGList {
		for _, neg := range list {
			if manager.namer.IsNEG(neg.Name) {
				negNames.Insert(neg.Name)
			}
		}
	}

	func() {
		manager.mu.Lock()
		defer manager.mu.Unlock()
		for _, portInfoMap := range manager.svcPortMap {
			for _, portInfo := range portInfoMap {
				negNames.Delete(portInfo.NegName)
			}
		}
	}()

	// This section includes a potential race condition between deleting neg here and users adds the neg annotation.
	// The worst outcome of the race condition is that neg is deleted in the end but user actually specifies a neg.
	// This would be resolved (sync neg) when the next endpoint update or resync arrives.
	// TODO: avoid race condition here
	for zone := range zoneNEGList {
		for _, name := range negNames.List() {
			if err := manager.ensureDeleteNetworkEndpointGroup(name, zone); err != nil {
				return fmt.Errorf("failed to delete NEG %q in %q: %v", name, zone, err)
			}
		}
	}
	return nil
}

// ensureDeleteNetworkEndpointGroup ensures neg is delete from zone
func (manager *syncerManager) ensureDeleteNetworkEndpointGroup(name, zone string) error {
	_, err := manager.cloud.GetNetworkEndpointGroup(name, zone)
	if err != nil {
		// Assume error is caused by not existing
		return nil
	}
	klog.V(2).Infof("Deleting NEG %q in %q.", name, zone)
	return manager.cloud.DeleteNetworkEndpointGroup(name, zone)
}

// getSyncerKey encodes a service namespace, name, service port and targetPort into a string key
func getSyncerKey(namespace, name string, port int32, targetPort string) negsyncer.NegSyncerKey {
	return negsyncer.NegSyncerKey{
		Namespace:  namespace,
		Name:       name,
		Port:       port,
		TargetPort: targetPort,
	}
}

func getServiceKey(namespace, name string) serviceKey {
	return serviceKey{
		namespace: namespace,
		name:      name,
	}
}
