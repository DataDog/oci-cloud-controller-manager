// Copyright 2024 Oracle and/or its affiliates. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package ipam

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/oracle/oci-cloud-controller-manager/pkg/oci/client"
	"github.com/pkg/errors"
	"go.uber.org/zap"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	coreinformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

const (
	// NodeNetworkUnavailableCondition is the condition type for node network availability
	NodeNetworkUnavailable = v1.NodeNetworkUnavailable

	// ResyncPeriod is how often the controller reconciles all nodes
	ResyncPeriod = 10 * time.Minute

	// MaxRetries is the maximum number of times a node will be retried before being dropped
	MaxRetries = 15
)

// CloudAllocator reads pod CIDR assignments from OCI secondary VNICs
// and syncs them to Node.Spec.PodCIDR. It follows the CloudAllocator pattern
// similar to GCP's implementation.
type CloudAllocator struct {
	// Kubernetes client
	client kubernetes.Interface

	// OCI client interface
	ociClient client.Interface

	// Node informer and lister
	nodeInformer  coreinformers.NodeInformer
	nodeLister    cache.Indexer
	nodeHasSynced cache.InformerSynced

	// Work queue for processing node events
	workqueue workqueue.RateLimitingInterface

	// cidrAllocators maps pod subnet ID to CIDR allocator
	// Key: subnet OCID, Value: CIDR allocator for that subnet
	cidrAllocators map[string]*SubnetCIDRAllocator
	allocatorLock  sync.RWMutex

	// Configuration
	nodeCIDRMaskSize int
	podSubnetIDs     map[string]string // map[availabilityDomain]subnetOCID
	autoAttachVNIC   bool
	vnicDisplayName  string

	// Logger
	logger *zap.SugaredLogger
}

// NewCloudAllocator creates a new CloudAllocator instance.
func NewCloudAllocator(
	client kubernetes.Interface,
	ociClient client.Interface,
	nodeInformer coreinformers.NodeInformer,
	nodeCIDRMaskSize int,
	podSubnetIDs map[string]string,
	autoAttachVNIC bool,
	vnicDisplayName string,
	logger *zap.SugaredLogger,
) *CloudAllocator {
	ca := &CloudAllocator{
		client:           client,
		ociClient:        ociClient,
		nodeInformer:     nodeInformer,
		nodeLister:       nodeInformer.Informer().GetIndexer(),
		nodeHasSynced:    nodeInformer.Informer().HasSynced,
		workqueue:        workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "ipam"),
		cidrAllocators:   make(map[string]*SubnetCIDRAllocator),
		nodeCIDRMaskSize: nodeCIDRMaskSize,
		podSubnetIDs:     podSubnetIDs,
		autoAttachVNIC:   autoAttachVNIC,
		vnicDisplayName:  vnicDisplayName,
		logger:           logger.With("controller", "ipam-cloud-allocator"),
	}

	// Set up event handlers for node changes
	nodeInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    ca.enqueueNode,
		UpdateFunc: ca.updateNode,
		DeleteFunc: ca.deleteNode,
	})

	return ca
}

// Run starts the CloudAllocator controller.
func (ca *CloudAllocator) Run(ctx context.Context, workers int) error {
	defer ca.workqueue.ShutDown()

	ca.logger.Info("Starting OCI IPAM CloudAllocator controller")

	// Wait for caches to sync
	ca.logger.Info("Waiting for node informer cache to sync")
	if !cache.WaitForCacheSync(ctx.Done(), ca.nodeHasSynced) {
		return errors.New("failed to wait for node cache to sync")
	}

	// Initialize CIDR allocators for each pod subnet
	if err := ca.initializeCIDRAllocators(ctx); err != nil {
		return errors.Wrap(err, "failed to initialize CIDR allocators")
	}

	// Reconcile existing nodes to populate CIDR allocators with existing allocations
	if err := ca.reconcileExistingNodes(ctx); err != nil {
		ca.logger.With("error", err).Warn("Failed to reconcile existing nodes, some CIDR conflicts may occur")
	}

	// Start worker goroutines
	ca.logger.With("workers", workers).Info("Starting workers")
	for i := 0; i < workers; i++ {
		go wait.UntilWithContext(ctx, ca.runWorker, time.Second)
	}

	ca.logger.Info("OCI IPAM CloudAllocator controller started")

	// Wait until context is cancelled
	<-ctx.Done()
	ca.logger.Info("Shutting down OCI IPAM CloudAllocator controller")

	return nil
}

// initializeCIDRAllocators creates CIDR allocators for each configured pod subnet.
func (ca *CloudAllocator) initializeCIDRAllocators(ctx context.Context) error {
	ca.logger.Info("Initializing CIDR allocators for pod subnets")

	for ad, subnetID := range ca.podSubnetIDs {
		// Get subnet details from OCI
		subnet, err := ca.ociClient.Networking(nil).GetSubnet(ctx, subnetID)
		if err != nil {
			return errors.Wrapf(err, "failed to get subnet %s for AD %s", subnetID, ad)
		}

		// Parse subnet CIDR
		subnetCIDR, err := ParseCIDR(*subnet.CidrBlock)
		if err != nil {
			return errors.Wrapf(err, "failed to parse subnet CIDR %s", *subnet.CidrBlock)
		}

		// Create CIDR allocator for this subnet
		cidrSet, err := newCIDRSet(subnetCIDR, ca.nodeCIDRMaskSize)
		if err != nil {
			return errors.Wrapf(err, "failed to create CIDR set for subnet %s", subnetID)
		}

		allocator := &SubnetCIDRAllocator{
			SubnetID:   subnetID,
			SubnetCIDR: subnetCIDR,
			NodeMask:   ca.nodeCIDRMaskSize,
			CIDRSet:    cidrSet,
		}

		ca.allocatorLock.Lock()
		ca.cidrAllocators[subnetID] = allocator
		ca.allocatorLock.Unlock()

		ca.logger.With(
			"availabilityDomain", ad,
			"subnetID", subnetID,
			"subnetCIDR", subnetCIDR.String(),
			"nodeMask", ca.nodeCIDRMaskSize,
		).Info("Initialized CIDR allocator for pod subnet")
	}

	return nil
}

// reconcileExistingNodes processes all existing nodes to populate CIDR allocators
// with existing allocations, preventing conflicts.
func (ca *CloudAllocator) reconcileExistingNodes(ctx context.Context) error {
	ca.logger.Info("Reconciling existing nodes with CIDR allocations")

	nodes, err := ca.client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return errors.Wrap(err, "failed to list nodes")
	}

	for _, node := range nodes.Items {
		if node.Spec.PodCIDR == "" {
			continue
		}

		// Parse the node's pod CIDR
		_, podCIDR, err := net.ParseCIDR(node.Spec.PodCIDR)
		if err != nil {
			ca.logger.With("node", node.Name, "podCIDR", node.Spec.PodCIDR, "error", err).Warn("Failed to parse existing pod CIDR")
			continue
		}

		// Find which subnet this CIDR belongs to and mark it as occupied
		if err := ca.occupyExistingCIDR(ctx, &node, podCIDR); err != nil {
			ca.logger.With("node", node.Name, "podCIDR", podCIDR.String(), "error", err).Warn("Failed to occupy existing CIDR")
		}
	}

	ca.logger.With("nodeCount", len(nodes.Items)).Info("Reconciliation of existing nodes complete")
	return nil
}

// occupyExistingCIDR marks an existing CIDR as occupied in the appropriate allocator.
func (ca *CloudAllocator) occupyExistingCIDR(ctx context.Context, node *v1.Node, cidr *net.IPNet) error {
	// Get availability domain from node labels
	availabilityDomain, ok := node.Labels["topology.kubernetes.io/zone"]
	if !ok {
		// Fall back to deprecated label
		availabilityDomain, ok = node.Labels["failure-domain.beta.kubernetes.io/zone"]
		if !ok {
			return errors.New("node does not have availability domain label (topology.kubernetes.io/zone)")
		}
	}

	// Get the pod subnet for this node's availability domain
	// Try exact match first
	subnetID, ok := ca.podSubnetIDs[availabilityDomain]
	if !ok {
		// If not found, try with realm prefix (format: "realm:AD-NAME")
		for configAD, configSubnetID := range ca.podSubnetIDs {
			if parts := strings.Split(configAD, ":"); len(parts) == 2 && parts[1] == availabilityDomain {
				subnetID = configSubnetID
				ok = true
				break
			}
		}
		if !ok {
			return errors.Errorf("no pod subnet configured for availability domain %s", availabilityDomain)
		}
	}

	// Get the allocator for this subnet
	ca.allocatorLock.RLock()
	allocator, ok := ca.cidrAllocators[subnetID]
	ca.allocatorLock.RUnlock()

	if !ok {
		return errors.Errorf("no CIDR allocator found for subnet %s", subnetID)
	}

	// Mark the CIDR as occupied
	if err := allocator.CIDRSet.Occupy(cidr); err != nil {
		return errors.Wrap(err, "failed to occupy CIDR")
	}

	ca.logger.With("node", node.Name, "podCIDR", cidr.String(), "subnetID", subnetID).Debug("Marked existing pod CIDR as occupied")
	return nil
}

// runWorker is a long-running function that will continually call processNextWorkItem
// to read and process a message on the workqueue.
func (ca *CloudAllocator) runWorker(ctx context.Context) {
	for ca.processNextWorkItem(ctx) {
	}
}

// processNextWorkItem reads a single work item off the workqueue and processes it.
func (ca *CloudAllocator) processNextWorkItem(ctx context.Context) bool {
	obj, shutdown := ca.workqueue.Get()
	if shutdown {
		return false
	}

	err := func(obj interface{}) error {
		defer ca.workqueue.Done(obj)

		key, ok := obj.(string)
		if !ok {
			ca.workqueue.Forget(obj)
			ca.logger.With("object", obj).Warn("Expected string in workqueue but got something else")
			return nil
		}

		// Process the node
		if err := ca.processNode(ctx, key); err != nil {
			// Requeue the item if processing failed
			if ca.workqueue.NumRequeues(key) < MaxRetries {
				ca.workqueue.AddRateLimited(key)
				return errors.Wrapf(err, "error processing node %s, requeuing", key)
			}

			// Max retries reached, drop the item
			ca.workqueue.Forget(key)
			ca.logger.With("node", key, "error", err).Error("Dropping node after max retries")
			return nil
		}

		// Successfully processed, forget the item
		ca.workqueue.Forget(key)
		return nil
	}(obj)

	if err != nil {
		ca.logger.With("error", err).Error("Error processing work item")
	}

	return true
}

// processNode processes a single node, allocating a pod CIDR if needed.
func (ca *CloudAllocator) processNode(ctx context.Context, key string) error {
	// Get the node from the cache
	obj, exists, err := ca.nodeLister.GetByKey(key)
	if err != nil {
		return errors.Wrapf(err, "failed to get node %s from cache", key)
	}

	if !exists {
		// Node was deleted, nothing to do
		ca.logger.With("node", key).Debug("Node no longer exists")
		return nil
	}

	node := obj.(*v1.Node)

	// Check if node already has a pod CIDR
	if node.Spec.PodCIDR != "" {
		ca.logger.With("node", node.Name, "podCIDR", node.Spec.PodCIDR).Debug("Node already has pod CIDR")
		return nil
	}

	ca.logger.With("node", node.Name).Info("Processing node for CIDR allocation")

	// Allocate a pod CIDR for this node
	podCIDR, subnetID, err := ca.allocatePodCIDRForNode(ctx, node)
	if err != nil {
		return errors.Wrapf(err, "failed to allocate pod CIDR for node %s", node.Name)
	}

	// Update the node with the allocated pod CIDR
	if err := ca.updateNodePodCIDR(ctx, node, podCIDR); err != nil {
		// Failed to update node, release the CIDR
		ca.releaseCIDR(subnetID, podCIDR)
		return errors.Wrapf(err, "failed to update node %s with pod CIDR", node.Name)
	}

	ca.logger.With("node", node.Name, "podCIDR", podCIDR).Info("Successfully allocated and assigned pod CIDR to node")
	return nil
}

// allocatePodCIDRForNode allocates a pod CIDR for the given node from the appropriate subnet.
func (ca *CloudAllocator) allocatePodCIDRForNode(ctx context.Context, node *v1.Node) (string, string, error) {
	// Get availability domain from node labels (set by kubelet/cloud provider)
	// Standard Kubernetes topology label
	availabilityDomain, ok := node.Labels["topology.kubernetes.io/zone"]
	if !ok {
		// Fall back to deprecated label
		availabilityDomain, ok = node.Labels["failure-domain.beta.kubernetes.io/zone"]
		if !ok {
			return "", "", errors.New("node does not have availability domain label (topology.kubernetes.io/zone), will retry")
		}
	}

	ca.logger.With(
		"node", node.Name,
		"availabilityDomain", availabilityDomain,
	).Debug("Processing node for pod CIDR allocation")

	// Get the pod subnet ID for this availability domain
	// Try exact match first
	subnetID, ok := ca.podSubnetIDs[availabilityDomain]
	if !ok {
		// If not found, try with realm prefix (format: "realm:AD-NAME")
		// This handles cases where config uses full format but label has short format
		for configAD, configSubnetID := range ca.podSubnetIDs {
			// Check if the config AD ends with the node's AD (after the colon)
			if parts := strings.Split(configAD, ":"); len(parts) == 2 && parts[1] == availabilityDomain {
				subnetID = configSubnetID
				ok = true
				ca.logger.With(
					"configuredAD", configAD,
					"nodeAD", availabilityDomain,
					"subnetID", subnetID,
				).Debug("Matched availability domain with realm prefix")
				break
			}
		}
		if !ok {
			return "", "", errors.Errorf("no pod subnet configured for availability domain %s", availabilityDomain)
		}
	}

	// Get the CIDR allocator for this subnet
	ca.allocatorLock.RLock()
	allocator, ok := ca.cidrAllocators[subnetID]
	ca.allocatorLock.RUnlock()

	if !ok {
		return "", "", errors.Errorf("no CIDR allocator found for subnet %s", subnetID)
	}

	// Allocate next available CIDR
	cidr, err := allocator.CIDRSet.AllocateNext()
	if err != nil {
		return "", "", errors.Wrap(err, "failed to allocate CIDR from subnet")
	}

	ca.logger.With(
		"node", node.Name,
		"availabilityDomain", availabilityDomain,
		"subnetID", subnetID,
		"allocatedCIDR", cidr.String(),
	).Info("Allocated pod CIDR for node")

	return cidr.String(), subnetID, nil
}

// updateNodePodCIDR updates the node's Spec.PodCIDR field and sets the NodeNetworkUnavailable condition to False.
func (ca *CloudAllocator) updateNodePodCIDR(ctx context.Context, node *v1.Node, podCIDR string) error {
	// Create a patch to update the node
	patch := fmt.Sprintf(`{"spec":{"podCIDR":"%s","podCIDRs":["%s"]}}`, podCIDR, podCIDR)

	_, err := ca.client.CoreV1().Nodes().Patch(
		ctx,
		node.Name,
		types.StrategicMergePatchType,
		[]byte(patch),
		metav1.PatchOptions{},
	)

	if err != nil {
		return errors.Wrap(err, "failed to patch node with pod CIDR")
	}

	// Update the NodeNetworkUnavailable condition to False
	// This signals that the node's network is properly configured
	if err := ca.updateNodeNetworkCondition(ctx, node); err != nil {
		ca.logger.With("node", node.Name, "error", err).Warn("Failed to update node network condition")
		// Don't return error here as the CIDR was successfully assigned
	}

	return nil
}

// updateNodeNetworkCondition sets the NodeNetworkUnavailable condition to False.
func (ca *CloudAllocator) updateNodeNetworkCondition(ctx context.Context, node *v1.Node) error {
	// Get the latest version of the node
	currentNode, err := ca.client.CoreV1().Nodes().Get(ctx, node.Name, metav1.GetOptions{})
	if err != nil {
		return errors.Wrap(err, "failed to get node")
	}

	// Check if condition already exists and is False
	for _, condition := range currentNode.Status.Conditions {
		if condition.Type == NodeNetworkUnavailable && condition.Status == v1.ConditionFalse {
			// Already set correctly
			return nil
		}
	}

	// Create the condition patch
	now := metav1.Now()
	condition := v1.NodeCondition{
		Type:               NodeNetworkUnavailable,
		Status:             v1.ConditionFalse,
		Reason:             "RouteCreated",
		Message:            "OCI IPAM allocated pod CIDR",
		LastTransitionTime: now,
		LastHeartbeatTime:  now,
	}

	// Find existing condition index
	conditionIndex := -1
	for i, c := range currentNode.Status.Conditions {
		if c.Type == NodeNetworkUnavailable {
			conditionIndex = i
			break
		}
	}

	if conditionIndex >= 0 {
		// Update existing condition
		patch := fmt.Sprintf(`{"status":{"conditions":[{"type":"%s","status":"%s","reason":"%s","message":"%s","lastTransitionTime":"%s","lastHeartbeatTime":"%s"}]}}`,
			condition.Type, condition.Status, condition.Reason, condition.Message,
			condition.LastTransitionTime.Format(time.RFC3339), condition.LastHeartbeatTime.Format(time.RFC3339))

		_, err = ca.client.CoreV1().Nodes().PatchStatus(ctx, node.Name, []byte(patch))
	} else {
		// Append new condition
		currentNode.Status.Conditions = append(currentNode.Status.Conditions, condition)
		_, err = ca.client.CoreV1().Nodes().UpdateStatus(ctx, currentNode, metav1.UpdateOptions{})
	}

	return err
}

// releaseCIDR releases a CIDR back to the allocator pool.
func (ca *CloudAllocator) releaseCIDR(subnetID string, podCIDR string) {
	ca.allocatorLock.RLock()
	allocator, ok := ca.cidrAllocators[subnetID]
	ca.allocatorLock.RUnlock()

	if !ok {
		ca.logger.With("subnetID", subnetID).Warn("No allocator found for subnet, cannot release CIDR")
		return
	}

	cidr, err := ParseCIDR(podCIDR)
	if err != nil {
		ca.logger.With("podCIDR", podCIDR, "error", err).Warn("Failed to parse CIDR for release")
		return
	}

	if err := allocator.CIDRSet.Release(cidr); err != nil {
		ca.logger.With("podCIDR", podCIDR, "error", err).Warn("Failed to release CIDR")
		return
	}

	ca.logger.With("podCIDR", podCIDR, "subnetID", subnetID).Debug("Released CIDR back to pool")
}

// enqueueNode adds a node to the workqueue.
func (ca *CloudAllocator) enqueueNode(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		ca.logger.With("error", err).Error("Failed to get key for node")
		return
	}
	ca.workqueue.Add(key)
}

// updateNode handles node update events.
func (ca *CloudAllocator) updateNode(old, new interface{}) {
	oldNode := old.(*v1.Node)
	newNode := new.(*v1.Node)

	// Only process if the node doesn't have a pod CIDR yet
	if newNode.Spec.PodCIDR == "" && oldNode.Spec.PodCIDR == "" {
		ca.enqueueNode(new)
	}
}

// deleteNode handles node deletion events.
func (ca *CloudAllocator) deleteNode(obj interface{}) {
	node, ok := obj.(*v1.Node)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			ca.logger.With("object", obj).Error("Error decoding object tombstone")
			return
		}
		node, ok = tombstone.Obj.(*v1.Node)
		if !ok {
			ca.logger.With("object", obj).Error("Error decoding object tombstone, invalid type")
			return
		}
	}

	// Release the node's CIDR if it had one
	if node.Spec.PodCIDR != "" {
		ca.logger.With("node", node.Name, "podCIDR", node.Spec.PodCIDR).Info("Node deleted, releasing CIDR")

		// Get availability domain from node labels
		availabilityDomain, ok := node.Labels["topology.kubernetes.io/zone"]
		if !ok {
			// Fall back to deprecated label
			availabilityDomain, ok = node.Labels["failure-domain.beta.kubernetes.io/zone"]
			if !ok {
				ca.logger.With("node", node.Name).Warn("Node does not have availability domain label, cannot release CIDR")
				return
			}
		}

		// Get the pod subnet for this availability domain
		subnetID, ok := ca.podSubnetIDs[availabilityDomain]
		if !ok {
			// Try with realm prefix
			for configAD, configSubnetID := range ca.podSubnetIDs {
				if parts := strings.Split(configAD, ":"); len(parts) == 2 && parts[1] == availabilityDomain {
					subnetID = configSubnetID
					ok = true
					break
				}
			}
		}

		if ok {
			ca.releaseCIDR(subnetID, node.Spec.PodCIDR)
		} else {
			ca.logger.With("node", node.Name, "availabilityDomain", availabilityDomain).Warn("No pod subnet configured for availability domain, cannot release CIDR")
		}
	}
}

// getInstanceIDFromProviderID extracts the instance OCID from a Kubernetes provider ID.
// Provider ID format: <provider-name>://<instance-ocid>
func getInstanceIDFromProviderID(providerID string) (string, error) {
	if providerID == "" {
		return "", errors.New("provider ID is empty")
	}

	// Expected format: oci://<instance-ocid>
	const prefix = "oci://"
	if len(providerID) <= len(prefix) {
		return "", errors.Errorf("invalid provider ID format: %s", providerID)
	}

	instanceID := providerID[len(prefix):]
	if instanceID == "" {
		return "", errors.Errorf("instance ID is empty in provider ID: %s", providerID)
	}

	return instanceID, nil
}
