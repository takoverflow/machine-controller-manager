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

This file was copied and modified from the kubernetes/kubernetes project
https://github.com/kubernetes/kubernetes/release-1.8/pkg/controller/replicaset/replica_set.go

Modifications Copyright SAP SE or an SAP affiliate company and Gardener contributors
*/

// Package controller is used to provide the core functionalities of machine-controller-manager
package controller

import (
	"context"
	"errors"
	"fmt"
	"github.com/gardener/machine-controller-manager/pkg/util/provider/machineutils"
	"reflect"
	"sort"
	"sync"
	"time"

	v1alpha1client "github.com/gardener/machine-controller-manager/pkg/client/clientset/versioned/typed/machine/v1alpha1"
	v1alpha1listers "github.com/gardener/machine-controller-manager/pkg/client/listers/machine/v1alpha1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	errorsutil "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/client-go/util/retry"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/integer"

	"k8s.io/klog/v2"

	"github.com/gardener/machine-controller-manager/pkg/apis/machine"
	"github.com/gardener/machine-controller-manager/pkg/apis/machine/v1alpha1"
	"github.com/gardener/machine-controller-manager/pkg/apis/machine/validation"
)

const (
	// BurstReplicas - Realistic value of the burstReplica field for the machine set manager based off
	// performance requirements for kubernetes 1.0.
	BurstReplicas = 100

	// The number of times we retry updating a MachineSet's status.
	statusUpdateRetries = 1

	// The number of times we retry updating finalizer.
	finalizerUpdateRetries = 2

	// Kind for the machineSet
	machineSetKind = "MachineSet"
)

var (
	staleMachinesRemoved     = &staleMachinesRemovedCounter{}
	controllerKindMachineSet = v1alpha1.SchemeGroupVersion.WithKind("MachineSet")
)

// getMachineMachineSets returns the MachineSets matching the given Machine.
func (c *controller) getMachineMachineSets(machine *v1alpha1.Machine) ([]*v1alpha1.MachineSet, error) {

	if len(machine.Labels) == 0 {
		err := errors.New("No MachineSets found for machine because it has no labels")
		klog.V(4).Info(err, ": ", machine.Name)
		return nil, err
	}

	list, err := c.machineSetLister.List(labels.Everything())
	if err != nil {
		return nil, err
	}

	var machineSets []*v1alpha1.MachineSet
	for _, machineSet := range list {
		if machineSet.Namespace != machine.Namespace {
			continue
		}
		selector, err := metav1.LabelSelectorAsSelector(machineSet.Spec.Selector)
		if err != nil {
			klog.Errorf("Invalid selector: %v", err)
			return nil, err
		}

		// If a MachineSet with a nil or empty selector creeps in, it should match nothing, not everything.
		if selector.Empty() || !selector.Matches(labels.Set(machine.Labels)) {
			continue
		}
		machineSets = append(machineSets, machineSet)
	}

	if len(machineSets) == 0 {
		err := errors.New("No MachineSets found for machine doesn't have matching labels")
		klog.V(4).Info(err, ": ", machine.Name)
		return nil, err
	}

	return machineSets, nil
}

// resolveMachineSetControllerRef returns the controller referenced by a ControllerRef,
// or nil if the ControllerRef could not be resolved to a matching controller
// of the correct Kind.
func (c *controller) resolveMachineSetControllerRef(namespace string, controllerRef *metav1.OwnerReference) *v1alpha1.MachineSet {
	// We can't look up by UID, so look up by Name and then verify UID.
	// Don't even try to look up by Name if it's the wrong Kind.
	if controllerRef.Kind != machineSetKind { // TOCheck
		return nil
	}
	machineSet, err := c.machineSetLister.MachineSets(namespace).Get(controllerRef.Name)
	if err != nil {
		return nil
	}
	if machineSet.UID != controllerRef.UID {
		// The controller we found with this Name is not the same one that the
		// ControllerRef points to.
		return nil
	}
	return machineSet
}

// callback when MachineSet is updated
func (c *controller) machineSetUpdate(old, cur interface{}) {
	oldMachineSet := old.(*v1alpha1.MachineSet)
	currentMachineSet := cur.(*v1alpha1.MachineSet)

	// You might imagine that we only really need to enqueue the
	// machine set when Spec changes, but it is safer to sync any
	// time this function is triggered. That way a full informer
	// resync can requeue any machine set that don't yet have machines
	// but whose last attempts at creating a machine have failed (since
	// we don't block on creation of machines) instead of those
	// machine sets stalling indefinitely. Enqueueing every time
	// does result in some spurious syncs (like when Status.Replica
	// is updated and the watch notification from it retriggers
	// this function), but in general extra resyncs shouldn't be
	// that bad as MachineSets that haven't met expectations yet won't
	// sync, and all the listing is done using local stores.
	if oldMachineSet.Spec.Replicas != currentMachineSet.Spec.Replicas {
		klog.V(3).Infof("%v updated. Desired machine count change: %d->%d", currentMachineSet.Name, oldMachineSet.Spec.Replicas, currentMachineSet.Spec.Replicas)
	}
	c.enqueueMachineSet(currentMachineSet)
}

// When a machine is created, enqueue the machine set that manages it and update its expectations.
func (c *controller) addMachineToMachineSet(obj interface{}) {
	machine := obj.(*v1alpha1.Machine)

	if machine.DeletionTimestamp != nil {
		// on a restart of the controller manager, it's possible a new machine shows up in a state that
		// is already pending deletion. Prevent the machine from being a creation observation.
		c.deleteMachineToMachineSet(machine)
		return
	}

	// If it has a ControllerRef, that's all that matters.
	if controllerRef := metav1.GetControllerOf(machine); controllerRef != nil {
		machineSet := c.resolveMachineSetControllerRef(machine.Namespace, controllerRef)
		if machineSet == nil {
			return
		}
		machineSetKey, err := KeyFunc(machineSet)
		if err != nil {
			return
		}
		klog.V(4).Infof("Machine %s created: %#v.", machine.Name, machine)
		c.expectations.CreationObserved(machineSetKey)
		c.enqueueMachineSet(machineSet)
		return
	}

	// Otherwise, it's an orphan. Get a list of all matching MachineSets and sync
	// them to see if anyone wants to adopt it.
	// DO NOT observe creation because no controller should be waiting for an
	// orphan.
	machineSets, err := c.getMachineMachineSets(machine)
	if err != nil {
		return
	} else if len(machineSets) == 0 {
		return
	}

	klog.V(4).Infof("Orphan Machine %s created: %#v.", machine.Name, machine)
	for _, machineSet := range machineSets {
		c.enqueueMachineSet(machineSet)
	}
}

// When a machine is updated, figure out what machine set/s manage it and wake them
// up. If the labels of the machine have changed we need to awaken both the old
// and new machine set. old and cur must be *v1alpha1.Machine types.
func (c *controller) updateMachineToMachineSet(old, cur interface{}) {
	curMachine := cur.(*v1alpha1.Machine)
	oldMachine := old.(*v1alpha1.Machine)
	if curMachine.ResourceVersion == oldMachine.ResourceVersion {
		// Periodic resync will send update events for all known machines.
		// Two different versions of the same machine will always have different RVs.
		return
	}

	labelChanged := !reflect.DeepEqual(curMachine.Labels, oldMachine.Labels)
	if curMachine.DeletionTimestamp != nil {
		// when a machine is deleted gracefully it's deletion timestamp is first modified to reflect a grace period,
		// and after such time has passed, the kubelet actually deletes it from the store. We receive an update
		// for modification of the deletion timestamp and expect an rs to create more replicas asap, not wait
		// until the kubelet actually deletes the machine. This is different from the Phase of a machine changing, because
		// an rs never initiates a phase change, and so is never asleep waiting for the same.
		c.deleteMachineToMachineSet(curMachine)
		if labelChanged {
			// we don't need to check the oldMachine.DeletionTimestamp because DeletionTimestamp cannot be unset.
			c.deleteMachineToMachineSet(oldMachine)
		}
		return
	}

	curControllerRef := metav1.GetControllerOf(curMachine)
	oldControllerRef := metav1.GetControllerOf(oldMachine)
	controllerRefChanged := !reflect.DeepEqual(curControllerRef, oldControllerRef)
	if controllerRefChanged && oldControllerRef != nil {
		// The ControllerRef was changed. Sync the old controller, if any.
		if machineSet := c.resolveMachineSetControllerRef(oldMachine.Namespace, oldControllerRef); machineSet != nil {
			c.enqueueMachineSet(machineSet)
		}
	}

	// If it has a ControllerRef, that's all that matters.
	if curControllerRef != nil {
		machineSet := c.resolveMachineSetControllerRef(curMachine.Namespace, curControllerRef)
		if machineSet == nil {
			return
		}
		klog.V(4).Infof("Machine %s updated, objectMeta %+v -> %+v.", curMachine.Name, oldMachine.ObjectMeta, curMachine.ObjectMeta)
		c.enqueueMachineSet(machineSet)
		return
	}

	// Otherwise, it's an orphan. If anything changed, sync matching controllers
	// to see if anyone wants to adopt it now.
	if labelChanged || controllerRefChanged {
		machineSets, err := c.getMachineMachineSets(curMachine)
		if err != nil {
			return
		} else if len(machineSets) == 0 {
			return
		}
		klog.V(4).Infof("Orphan Machine %s updated, objectMeta %+v -> %+v.", curMachine.Name, oldMachine.ObjectMeta, curMachine.ObjectMeta)
		for _, machineSet := range machineSets {
			c.enqueueMachineSet(machineSet)
		}
	}

}

// When a machine is deleted, enqueue the machine set that manages the machine and update its expectations.
// obj could be an *v1alpha1.Machine, or a DeletionFinalStateUnknown marker item.
func (c *controller) deleteMachineToMachineSet(obj interface{}) {
	machine, ok := obj.(*v1alpha1.Machine)

	// When a delete is dropped, the relist will notice a machine in the store not
	// in the list, leading to the insertion of a tombstone object which contains
	// the deleted key/value. Note that this value might be stale. If the machine
	// changed labels the new MachineSet will not be woken up till the periodic resync.
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("couldn't get object from tombstone %+v", obj))
			return
		}
		machine, ok = tombstone.Obj.(*v1alpha1.Machine)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("tombstone contained object that is not a machine %#v", obj))
			return
		}
	}

	controllerRef := metav1.GetControllerOf(machine)
	if controllerRef == nil {
		// No controller should care about orphans being deleted.
		return
	}
	machineSet := c.resolveMachineSetControllerRef(machine.Namespace, controllerRef)
	if machineSet == nil {
		return
	}
	machineSetKey, err := KeyFunc(machineSet)
	if err != nil {
		return
	}
	klog.V(4).Infof("Machine %s/%s deleted through %v, timestamp %+v: %#v.", machine.Namespace, machine.Name, utilruntime.GetCaller(), machine.DeletionTimestamp, machine)
	c.expectations.DeletionObserved(machineSetKey, MachineKey(machine))
	c.enqueueMachineSet(machineSet)
}

// obj could be an *extensions.MachineSet, or a DeletionFinalStateUnknown marker item.
func (c *controller) enqueueMachineSet(obj interface{}) {
	key, err := KeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", obj, err))
		return
	}
	c.machineSetQueue.Add(key)
}

// obj could be an *extensions.MachineSet, or a DeletionFinalStateUnknown marker item.
func (c *controller) enqueueMachineSetAfter(obj interface{}, after time.Duration) {
	key, err := KeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", obj, err))
		return
	}
	c.machineSetQueue.AddAfter(key, after)
}

// manageReplicas checks and updates replicas for the given MachineSet.
// Does NOT modify <filteredMachines>.
// It will requeue the machine set in case of an error while creating/deleting machines.
func (c *controller) manageReplicas(ctx context.Context, allMachines []*v1alpha1.Machine, machineSet *v1alpha1.MachineSet) error {
	machineSetKey, err := KeyFunc(machineSet)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("Couldn't get key for %v %#v: %v", machineSet.Kind, machineSet, err))
		return nil
	}

	var activeMachines, staleMachines []*v1alpha1.Machine
	for _, m := range allMachines {
		if machineutils.IsMachineFailed(m) || machineutils.IsMachineTriggeredForDeletion(m) {
			staleMachines = append(staleMachines, m)
		} else if machineutils.IsMachineActive(m) {
			activeMachines = append(activeMachines, m)
		}
	}

	if len(staleMachines) >= 1 {
		klog.V(3).Infof("Deleting stale machines %s", getMachineKeys(staleMachines))
	}
	if err := c.terminateMachines(ctx, staleMachines, machineSet); err != nil {
		// TODO: proper error handling needs to happen here
		klog.Errorf("failed to terminate stale machines for machineset %s: %v", machineSet.Name, err)
	}

	diff := len(activeMachines) - int(machineSet.Spec.Replicas)
	klog.V(4).Infof("Difference between current active replicas and desired replicas - %d", diff)

	if diff < 0 {
		// If MachineSet is frozen and no deletion timestamp, don't process it
		if machineSet.Labels["freeze"] == "True" && machineSet.DeletionTimestamp == nil {
			klog.V(2).Infof("MachineSet %q is frozen, and hence not processing", machineSet.Name)
			return nil
		}

		diff *= -1
		if diff > BurstReplicas {
			diff = BurstReplicas
		}
		// TODO: Track UIDs of creates just like deletes. The problem currently
		// is we'd need to wait on the result of a create to record the machine's
		// UID, which would require locking *across* the create, which will turn
		// into a performance bottleneck. We should generate a UID for the machine
		// beforehand and store it via ExpectCreations.
		if err := c.expectations.ExpectCreations(machineSetKey, diff); err != nil {
			// TODO: proper error handling needs to happen here
			klog.Errorf("failed expect creations for machineset %s: %v", machineSet.Name, err)
		}
		klog.V(2).Infof("Too few replicas for MachineSet %s, need %d, creating %d", machineSet.Name, (machineSet.Spec.Replicas), diff)
		// Batch the machine creates. Batch sizes start at SlowStartInitialBatchSize
		// and double with each successful iteration in a kind of "slow start".
		// This handles attempts to start large numbers of machines that would
		// likely all fail with the same error. For example a project with a
		// low quota that attempts to create a large number of machines will be
		// prevented from spamming the API service with the machine create requests
		// after one of its machines fails.  Conveniently, this also prevents the
		// event spam that those failures would generate.
		successfulCreations, err := slowStartBatch(diff, SlowStartInitialBatchSize, func() error {
			boolPtr := func(b bool) *bool { return &b }
			controllerRef := &metav1.OwnerReference{
				APIVersion:         controllerKindMachineSet.GroupVersion().String(), // #ToCheck
				Kind:               controllerKindMachineSet.Kind,                    // machineSet.Kind,
				Name:               machineSet.Name,
				UID:                machineSet.UID,
				BlockOwnerDeletion: boolPtr(true),
				Controller:         boolPtr(true),
			}
			err := c.machineControl.CreateMachinesWithControllerRef(ctx, machineSet.Namespace, &machineSet.Spec.Template, machineSet, controllerRef)
			if err != nil && apierrors.IsTimeout(err) {
				// Machine is created but its initialization has timed out.
				// If the initialization is successful eventually, the
				// controller will observe the creation via the informer.
				// If the initialization fails, or if the machine keeps
				// uninitialized for a long time, the informer will not
				// receive any update, and the controller will create a new
				// machine when the expectation expires.
				return nil
			}
			return err
		})

		// Any skipped machines that we never attempted to start shouldn't be expected.
		// The skipped machines will be retried later. The next controller resync will
		// retry the slow start process.
		if skippedMachines := diff - successfulCreations; skippedMachines > 0 {
			klog.V(2).Infof("Slow-start failure. Skipping creation of %d machines, decrementing expectations for %v %v/%v", skippedMachines, machineSet.Kind, machineSet.Namespace, machineSet.Name)
			for i := 0; i < skippedMachines; i++ {
				// Decrement the expected number of creates because the informer won't observe this machine
				c.expectations.CreationObserved(machineSetKey)
			}
		}
		return err
	} else if diff > 0 {
		if diff > BurstReplicas {
			diff = BurstReplicas
		}
		klog.V(2).Infof("Too many replicas for %v %s/%s, need %d, deleting %d", machineSet.Kind, machineSet.Namespace, machineSet.Name, (machineSet.Spec.Replicas), diff)

		logMachinesWithPriority1(activeMachines)
		machinesToDelete := getMachinesToDelete(activeMachines, diff)
		logMachinesToDelete(machinesToDelete)

		// Snapshot the UIDs (ns/name) of the machines we're expecting to see
		// deleted, so we know to record their expectations exactly once either
		// when we see it as an update of the deletion timestamp, or as a delete.
		// Note that if the labels on a machine/rs change in a way that the machine gets
		// orphaned, the rs will only wake up after the expectations have
		// expired even if other machines are deleted.
		if err := c.expectations.ExpectDeletions(machineSetKey, getMachineKeys(machinesToDelete)); err != nil {
			// TODO: proper error handling needs to happen here
			klog.Errorf("failed expect deletions for machineset %s: %v", machineSet.Name, err)
		}

		if err := c.terminateMachines(ctx, machinesToDelete, machineSet); err != nil {
			// TODO: proper error handling needs to happen here
			klog.Errorf("failed to terminate machines for machineset %s: %v", machineSet.Name, err)
		}
	}

	return nil
}

// syncMachineSet will sync the MachineSet with the given key if it has had its expectations fulfilled,
// meaning it did not expect to see any more of its machines created or deleted. This function is not meant to be
// invoked concurrently with the same key.
func (c *controller) reconcileClusterMachineSet(key string) error {

	ctx := context.Background()

	startTime := time.Now()
	klog.V(4).Infof("Start syncing machine set %q", key)
	defer func() {
		klog.V(4).Infof("Finished syncing machine set %q (%v)", key, time.Since(startTime))
	}()

	_, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}

	// Get the latest version of the machineSet so that we can avoid conflicts
	machineSet, err := c.controlMachineClient.MachineSets(c.namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		klog.V(4).Infof("%v has been deleted", key)
		c.expectations.DeleteExpectations(key)
		return nil
	}
	if err != nil {
		return err
	}

	// Validate MachineSet
	internalMachineSet := &machine.MachineSet{}
	err = c.internalExternalScheme.Convert(machineSet, internalMachineSet, nil)
	if err != nil {
		return err
	}
	validationerr := validation.ValidateMachineSet(internalMachineSet)
	if validationerr.ToAggregate() != nil && len(validationerr.ToAggregate().Errors()) > 0 {
		klog.V(2).Infof("Validation of MachineSet failed %s", validationerr.ToAggregate().Error())
		return nil
	}

	if machineSet.DeletionTimestamp == nil {
		// Manipulate finalizers
		if err := c.addMachineSetFinalizers(ctx, machineSet); err != nil {
			return err
		}
	}
	klog.V(3).Infof("Processing the machineset %q with replicas %d associated with machine class: %q", machineSet.Name, machineSet.Spec.Replicas, machineSet.Spec.MachineClass.Name)

	selector, err := metav1.LabelSelectorAsSelector(machineSet.Spec.Selector)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("Error converting machine selector to selector: %v", err))
		return nil
	}

	// list all machines to include the machines that don't match the rs`s selector
	// anymore but has the stale controller ref.
	// TODO: Do the List and Filter in a single pass, or use an index.
	filteredMachines, err := c.machineLister.List(labels.Everything())
	if err != nil {
		return err
	}

	// NOTE: filteredMachines are pointing to objects from cache - if you need to
	// modify them, you need to copy it first.
	filteredMachines, err = c.claimMachines(ctx, machineSet, selector, filteredMachines)
	if err != nil {
		return err
	}

	// syncMachinesNodeTemplates syncs the nodeTemplate with claimedMachines if any of the machine's nodeTemplate has changed.
	err = c.syncMachinesNodeTemplates(ctx, filteredMachines, machineSet)
	if err != nil {
		return err
	}

	// syncMachinesConfig syncs the config with claimedMachines if any of the machine's config has changed.
	err = c.syncMachinesConfig(ctx, filteredMachines, machineSet)
	if err != nil {
		return err
	}
	// syncMachinesClassKind syncs the classKind with claimedMachines if any of the machine's classKind has changed.
	err = c.syncMachinesClassKind(ctx, filteredMachines, machineSet)
	if err != nil {
		return err
	}

	// TODO: Fix working of expectations to reflect correct behaviour
	// machineSetNeedsSync := c.expectations.SatisfiedExpectations(key)
	var manageReplicasErr error

	if machineSet.DeletionTimestamp == nil {
		// manageReplicas is the core machineSet method where scale up/down occurs
		// It is not called when deletion timestamp is set
		manageReplicasErr = c.manageReplicas(ctx, filteredMachines, machineSet)

	} else if machineSet.DeletionTimestamp != nil {
		// When machineSet if triggered for deletion

		if len(filteredMachines) == 0 {
			// If machines backing a machineSet are zero,
			// remove the machineSetFinalizer
			if err := c.deleteMachineSetFinalizers(ctx, machineSet); err != nil {
				return err
			}
		} else if finalizers := sets.NewString(machineSet.Finalizers...); finalizers.Has(DeleteFinalizerName) {
			// Trigger deletion of machines backing the machineSet
			klog.V(3).Infof("Deleting all child machines as MachineSet %s has set deletionTimestamp", machineSet.Name)
			if err := c.terminateMachines(ctx, filteredMachines, machineSet); err != nil {
				// TODO: proper error handling needs to happen here
				klog.Errorf("failed terminate machines for machineset %s: %v", machineSet.Name, err)
			}
		}
	}

	machineSet = machineSet.DeepCopy()
	newStatus := calculateMachineSetStatus(machineSet, filteredMachines, manageReplicasErr)

	// Always updates status as machines come up or die.
	updatedMachineSet, err := updateMachineSetStatus(ctx, c.controlMachineClient, machineSet, newStatus)
	if err != nil {
		// Multiple things could lead to this update failing. Requeuing the machine set ensures
		// Returning an error causes a requeue without forcing a hotloop
		if !apierrors.IsNotFound(err) {
			klog.Errorf("Update machineSet %s failed with: %s", machineSet.Name, err)
		}
		return err
	}

	// Resync the MachineSet after 10 minutes to avoid missing out on missed out events
	defer c.enqueueMachineSetAfter(updatedMachineSet, 10*time.Minute)

	return manageReplicasErr
}

func (c *controller) claimMachines(ctx context.Context, machineSet *v1alpha1.MachineSet, selector labels.Selector, filteredMachines []*v1alpha1.Machine) ([]*v1alpha1.Machine, error) {
	// If any adoptions are attempted, we should first recheck for deletion with
	// an uncached quorum read sometime after listing Machines (see #42639).
	canAdoptFunc := RecheckDeletionTimestamp(func() (metav1.Object, error) {
		fresh, err := c.controlMachineClient.MachineSets(machineSet.Namespace).Get(ctx, machineSet.Name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		if fresh.UID != machineSet.UID {
			return nil, fmt.Errorf("original %v/%v MachineSet gone: got uid %v, wanted %v", machineSet.Namespace, machineSet.Name, fresh.UID, machineSet.UID)
		}
		return fresh, nil
	})
	cm := NewMachineControllerRefManager(c.machineControl, machineSet, selector, controllerKindMachineSet, canAdoptFunc)
	return cm.ClaimMachines(ctx, filteredMachines)
}

// slowStartBatch tries to call the provided function a total of 'count' times,
// starting slow to check for errors, then speeding up if calls succeed.
//
// It groups the calls into batches, starting with a group of initialBatchSize.
// Within each batch, it may call the function multiple times concurrently.
//
// If a whole batch succeeds, the next batch may get exponentially larger.
// If there are any failures in a batch, all remaining batches are skipped
// after waiting for the current batch to complete.
//
// It returns the number of successful calls to the function.
func slowStartBatch(count int, initialBatchSize int, fn func() error) (int, error) {
	remaining := count
	successes := 0
	for batchSize := integer.IntMin(remaining, initialBatchSize); batchSize > 0; batchSize = integer.IntMin(2*batchSize, remaining) {
		errCh := make(chan error, batchSize)
		defer close(errCh)

		var wg sync.WaitGroup
		wg.Add(batchSize)
		for i := 0; i < batchSize; i++ {
			go func() {
				defer wg.Done()
				if err := fn(); err != nil {
					errCh <- err
				}
			}()
		}
		wg.Wait()
		curSuccesses := batchSize - len(errCh)
		successes += curSuccesses
		if len(errCh) > 0 {
			return successes, <-errCh
		}
		remaining -= batchSize
	}
	return successes, nil
}

func getMachinesToDelete(filteredMachines []*v1alpha1.Machine, diff int) []*v1alpha1.Machine {
	// No need to sort machines if we are about to delete all of them.
	// diff will always be <= len(filteredMachines), so not need to handle > case.
	if diff < len(filteredMachines) {
		// Sort the machines in the order such that not-ready < ready, unscheduled
		// < scheduled, and pending < running. This ensures that we delete machines
		// in the earlier stages whenever possible.
		sort.Sort(ActiveMachines(filteredMachines))
	}
	return filteredMachines[:diff]
}

func getMachineKeys(machines []*v1alpha1.Machine) []string {
	machineKeys := make([]string, 0, len(machines))
	for _, machine := range machines {
		machineKeys = append(machineKeys, MachineKey(machine))
	}
	return machineKeys
}

func (c *controller) prepareMachineForDeletion(ctx context.Context, targetMachine *v1alpha1.Machine, machineSet *v1alpha1.MachineSet, wg *sync.WaitGroup, errCh chan<- error) {
	defer wg.Done()

	// Machine is already marked as 'to-be-deleted'
	if targetMachine.DeletionTimestamp != nil {
		return
	}

	machineSetKey, err := KeyFunc(machineSet)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("Couldn't get key for %v %#v: %v", machineSet.Kind, machineSet, err))
		return
	}

	err = c.machineControl.DeleteMachine(ctx, targetMachine.Namespace, targetMachine.Name, machineSet)
	if err != nil {
		// Decrement the expected number of deletes because the informer won't observe this deletion
		machineKey := MachineKey(targetMachine)
		klog.V(2).Infof("Failed to delete %v, decrementing expectations for %v %s/%s", machineKey, machineSet.Kind, machineSet.Namespace, machineSet.Name)
		c.expectations.DeletionObserved(machineSetKey, machineKey)
		errCh <- err
	} else {
		// successful delete of a Failed phase machine due to unhealthiness for too long, increments staleMachinesRemoved counter
		// note: call is blocking and thread safe as other worker threads might be updating the counter as well
		if machineutils.IsMachineFailed(targetMachine) && targetMachine.Status.LastOperation.Type == v1alpha1.MachineOperationHealthCheck {
			staleMachinesRemoved.increment()
		}
	}

	// Force trigger deletion to reflect in machine status
	lastOperation := v1alpha1.LastOperation{
		Description:    "Deleting machine from cloud provider",
		State:          "Processing",
		Type:           "Delete",
		LastUpdateTime: metav1.Now(),
	}
	currentStatus := v1alpha1.CurrentStatus{
		Phase:          v1alpha1.MachineTerminating,
		TimeoutActive:  false,
		LastUpdateTime: metav1.Now(),
	}
	if _, err := c.updateMachineStatus(ctx, targetMachine, lastOperation, currentStatus); err != nil {
		// TODO: proper error handling needs to happen here
		klog.Errorf("failed to update machine status for machine %s: %v", targetMachine.Name, err)
	}
	klog.V(2).Infof("Delete machine from machineset %q", targetMachine.Name)
}

func (c *controller) terminateMachines(ctx context.Context, inactiveMachines []*v1alpha1.Machine, machineSet *v1alpha1.MachineSet) error {
	var (
		wg                    sync.WaitGroup
		numOfInactiveMachines = len(inactiveMachines)
		errCh                 = make(chan error, numOfInactiveMachines)
	)
	defer close(errCh)

	wg.Add(numOfInactiveMachines)
	for _, machine := range inactiveMachines {
		go c.prepareMachineForDeletion(ctx, machine, machineSet, &wg, errCh)
	}
	wg.Wait()

	select {
	case err := <-errCh:
		// all errors have been reported before and they're likely to be the same, so we'll only return the first one we hit.
		if err != nil {
			return err
		}
	default:
	}

	return nil
}

/*
	SECTION
	Manipulate Finalizers
*/

func (c *controller) addMachineSetFinalizers(ctx context.Context, machineSet *v1alpha1.MachineSet) error {
	clone := machineSet.DeepCopy()

	if finalizers := sets.NewString(clone.Finalizers...); !finalizers.Has(DeleteFinalizerName) {
		finalizers.Insert(DeleteFinalizerName)
		if err := c.updateMachineSetFinalizers(ctx, clone, finalizers.List()); err != nil {
			return err
		}
	}
	return nil
}

func (c *controller) deleteMachineSetFinalizers(ctx context.Context, machineSet *v1alpha1.MachineSet) error {
	clone := machineSet.DeepCopy()

	if finalizers := sets.NewString(clone.Finalizers...); finalizers.Has(DeleteFinalizerName) {
		finalizers.Delete(DeleteFinalizerName)
		if err := c.updateMachineSetFinalizers(ctx, clone, finalizers.List()); err != nil {
			return err
		}
	}
	return nil
}

// updateMachineSetFinalizers tries to update the machineSet finalizers for finalizerUpdateRetries number of times
func (c *controller) updateMachineSetFinalizers(ctx context.Context, machineSet *v1alpha1.MachineSet, finalizers []string) error {
	var err error

	// Stop retrying if we exceed finalizerUpdateRetries - the machineSet will be requeued with rate limit
	for i := 0; i < finalizerUpdateRetries; i++ {
		// Get the latest version of the machineSet so that we can avoid conflicts
		machineSet, err = c.controlMachineClient.MachineSets(machineSet.Namespace).Get(ctx, machineSet.Name, metav1.GetOptions{})
		if err != nil {
			klog.V(3).Infof("Failed to fetch machineSet %q from API server, will retry. Error: %q", machineSet.Name, err.Error())
			return err
		}

		clone := machineSet.DeepCopy()
		clone.Finalizers = finalizers

		_, err = c.controlMachineClient.MachineSets(clone.Namespace).Update(ctx, clone, metav1.UpdateOptions{})

		if err == nil {
			return nil
		}
	}

	klog.Warning(fmt.Sprintf("Updating machineset %q failed at time %q with err: %q, requeuing", machineSet.Name, time.Now(), err.Error()))
	return err
}

func (c *controller) updateMachineStatus(
	ctx context.Context,
	machine *v1alpha1.Machine,
	lastOperation v1alpha1.LastOperation,
	currentStatus v1alpha1.CurrentStatus,
) (*v1alpha1.Machine, error) {
	// Get the latest version of the machine so that we can avoid conflicts
	latestMachine, err := c.controlMachineClient.Machines(machine.Namespace).Get(ctx, machine.Name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	clone := latestMachine.DeepCopy()

	clone.Status.LastOperation = lastOperation
	clone.Status.CurrentStatus = currentStatus
	if isMachineStatusEqual(clone.Status, machine.Status) {
		klog.V(3).Infof("Not updating the status of the machine object %q , as it is already same", clone.Name)
		return machine, nil
	}

	clone, err = c.controlMachineClient.Machines(clone.Namespace).UpdateStatus(ctx, clone, metav1.UpdateOptions{})
	if err != nil {
		// Keep retrying until update goes through
		klog.V(3).Infof("Warning: Updated failed, retrying, error: %q", err)
		return c.updateMachineStatus(ctx, machine, lastOperation, currentStatus)
	}
	return clone, nil
}

// isMachineStatusEqual checks if the status of 2 machines is same or not.
func isMachineStatusEqual(s1, s2 v1alpha1.MachineStatus) bool {
	tolerateTimeDiff := 30 * time.Minute
	s1Copy, s2Copy := s1.DeepCopy(), s2.DeepCopy()
	s1Copy.LastOperation.Description, s2Copy.LastOperation.Description = "", ""

	if (s1Copy.LastOperation.LastUpdateTime.Time.Before(time.Now().Add(tolerateTimeDiff * -1))) || (s2Copy.LastOperation.LastUpdateTime.Time.Before(time.Now().Add(tolerateTimeDiff * -1))) {
		return false
	}
	s1Copy.LastOperation.LastUpdateTime, s2Copy.LastOperation.LastUpdateTime = metav1.Time{}, metav1.Time{}

	if (s1Copy.CurrentStatus.LastUpdateTime.Time.Before(time.Now().Add(tolerateTimeDiff * -1))) || (s2Copy.CurrentStatus.LastUpdateTime.Time.Before(time.Now().Add(tolerateTimeDiff * -1))) {
		return false
	}
	s1Copy.CurrentStatus.LastUpdateTime, s2Copy.CurrentStatus.LastUpdateTime = metav1.Time{}, metav1.Time{}

	return apiequality.Semantic.DeepEqual(s1Copy.LastOperation, s2Copy.LastOperation) && apiequality.Semantic.DeepEqual(s1Copy.CurrentStatus, s2Copy.CurrentStatus)
}

// see https://github.com/kubernetes/kubernetes/issues/21479
type updateMachineFunc func(machine *v1alpha1.Machine) error

// UpdateMachineWithRetries updates a machine with given applyUpdate function. Note that machine not found error is ignored.
// The returned bool value can be used to tell if the machine is actually updated.
func UpdateMachineWithRetries(ctx context.Context, machineClient v1alpha1client.MachineInterface, machineLister v1alpha1listers.MachineLister, namespace, name string, applyUpdate updateMachineFunc) (*v1alpha1.Machine, error) {
	var machine *v1alpha1.Machine

	retryErr := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		var err error
		machine, err = machineLister.Machines(namespace).Get(name)
		if err != nil {
			return err
		}
		machine = machine.DeepCopy()
		// Apply the update, then attempt to push it to the apiserver.
		if applyErr := applyUpdate(machine); applyErr != nil {
			return applyErr
		}
		machine, err = machineClient.Update(ctx, machine, metav1.UpdateOptions{})
		return err
	})

	// Ignore the precondition violated error, this machine is already updated
	// with the desired label.
	if retryErr == errorsutil.ErrPreconditionViolated {
		klog.V(4).Infof("Machine %s precondition doesn't hold, skip updating it.", name)
		retryErr = nil
	}

	return machine, retryErr
}
