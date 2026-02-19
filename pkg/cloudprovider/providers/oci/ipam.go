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

package oci

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/oracle/oci-go-sdk/v65/core"
	"github.com/pkg/errors"
	"go.uber.org/zap"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	coreinformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

const (
	// NodeNetworkUnavailable is the condition type for node network availability
	NodeNetworkUnavailable = v1.NodeNetworkUnavailable

	// ResyncPeriod is how often the controller reconciles all nodes
	ResyncPeriod = 10 * time.Minute

	// MaxRetries is the maximum number of times a node will be retried before being dropped
	MaxRetries = 15
)

// IPAMController reads pod CIDR assignments from OCI secondary VNICs
// and syncs them to Node.Spec.PodCIDR. It follows the IPAMController pattern
// similar to GCP's implementation.
type IPAMController struct {
	cp *CloudProvider

	// Node informer and lister
	nodeInformer  coreinformers.NodeInformer
	nodeLister    cache.Indexer
	nodeHasSynced cache.InformerSynced

	// Work queue for processing node events
	workqueue workqueue.RateLimitingInterface

	// Logger
	logger *zap.SugaredLogger
}

// NewIPAMController creates a new IPAMController instance.
func NewIPAMController(
	cp *CloudProvider,
	nodeInformer coreinformers.NodeInformer,
) *IPAMController {
	ic := &IPAMController{
		cp:            cp,
		nodeInformer:  nodeInformer,
		nodeLister:    nodeInformer.Informer().GetIndexer(),
		nodeHasSynced: nodeInformer.Informer().HasSynced,
		workqueue:     workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "ipam"),
		logger:        cp.logger.With("controller", "ipam-cloud-allocator"),
	}

	// Set up event handlers for node changes
	_, _ = nodeInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    ic.enqueueNode,
		UpdateFunc: ic.updateNode,
	})

	return ic
}

// Run starts the IPAMController controller.
func (ic *IPAMController) Run(ctx context.Context, workers int) error {
	defer ic.workqueue.ShutDown()

	ic.logger.Info("Starting OCI IPAM IPAMController controller")

	// Wait for caches to sync
	ic.logger.Info("Waiting for node informer cache to sync")
	if !cache.WaitForCacheSync(ctx.Done(), ic.nodeHasSynced) {
		return errors.New("failed to wait for node cache to sync")
	}

	// Reconcile existing nodes to populate CIDR allocators with existing allocations
	if err := ic.reconcileExistingNodes(ctx); err != nil {
		ic.logger.With("error", err).Warn("Failed to reconcile existing nodes, some CIDR conflicts may occur")
	}

	// Start worker goroutines
	ic.logger.With("workers", workers).Info("Starting workers")
	for i := 0; i < workers; i++ {
		go wait.UntilWithContext(ctx, ic.runWorker, time.Second)
	}

	ic.logger.Info("OCI IPAM IPAMController controller started")

	// Wait until context is cancelled
	<-ctx.Done()
	ic.logger.Info("Shutting down OCI IPAM IPAMController controller")

	return nil
}

// reconcileExistingNodes processes all existing nodes to populate CIDR allocators
// with existing allocations, preventing conflicts.
func (ic *IPAMController) reconcileExistingNodes(ctx context.Context) error {
	ic.logger.Info("Reconciling existing nodes with CIDR allocations")

	nodes, err := ic.cp.kubeclient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return errors.Wrap(err, "failed to list nodes")
	}

	for _, node := range nodes.Items {
		err = ic.processNode(ctx, node.Name)
		if err != nil {
			ic.logger.With("node", node.Name, "error", err).Warn("Failed to process node")
			continue
		}
	}

	ic.logger.With("nodeCount", len(nodes.Items)).Info("Reconciliation of existing nodes complete")
	return nil
}

// runWorker is a long-running function that will continually call processNextWorkItem
// to read and process a message on the workqueue.
func (ic *IPAMController) runWorker(ctx context.Context) {
	for ic.processNextWorkItem(ctx) {
	}
}

// processNextWorkItem reads a single work item off the workqueue and processes it.
func (ic *IPAMController) processNextWorkItem(ctx context.Context) bool {
	obj, shutdown := ic.workqueue.Get()
	if shutdown {
		return false
	}

	err := func(obj interface{}) error {
		defer ic.workqueue.Done(obj)

		key, ok := obj.(string)
		if !ok {
			ic.workqueue.Forget(obj)
			ic.logger.With("object", obj).Warn("Expected string in workqueue but got something else")
			return nil
		}

		// Process the node
		if err := ic.processNode(ctx, key); err != nil {
			// Requeue the item if processing failed
			if ic.workqueue.NumRequeues(key) < MaxRetries {
				ic.workqueue.AddRateLimited(key)
				return errors.Wrapf(err, "error processing node %s, requeuing", key)
			}

			// Max retries reached, drop the item
			ic.workqueue.Forget(key)
			ic.logger.With("node", key, "error", err).Error("Dropping node after max retries")
			return nil
		}

		// Successfully processed, forget the item
		ic.workqueue.Forget(key)
		return nil
	}(obj)

	if err != nil {
		ic.logger.With("error", err).Error("Error processing work item")
	}

	return true
}

// processNode processes a single node, allocating a pod CIDR if needed.
func (ic *IPAMController) processNode(ctx context.Context, key string) error {
	// Get the node from the cache
	obj, exists, err := ic.nodeLister.GetByKey(key)
	if err != nil {
		return errors.Wrapf(err, "failed to get node %s from cache", key)
	}

	if !exists {
		// Node was deleted, nothing to do
		ic.logger.With("node", key).Debug("Node no longer exists")
		return nil
	}

	node := obj.(*v1.Node)

	// Check if node already has a pod CIDR
	if node.Spec.PodCIDR != "" {
		ic.logger.With("node", node.Name, "podCIDR", node.Spec.PodCIDR).Debug("Node already has pod CIDR")
		return nil
	}

	ic.logger.With("node", node.Name).Info("Processing node for CIDR allocation")

	// Allocate a pod CIDR for this node
	podCIDR, err := ic.allocatePodCIDRForNode(ctx, node)
	if err != nil {
		return errors.Wrapf(err, "failed to allocate pod CIDR for node %s", node.Name)
	}

	ic.logger.With("node", node.Name, "podCIDR", podCIDR).Info("Successfully allocated and assigned pod CIDR to node")
	return nil
}

// allocatePodCIDRForNode allocates a pod CIDR for the given node from the appropriate subnet.
func (ic *IPAMController) allocatePodCIDRForNode(ctx context.Context, node *v1.Node) (string, error) {
	ic.logger.With(
		"node", node.Name,
	).Debug("Processing node for pod CIDR allocation")

	// Get the instance ID from provider ID
	instanceID, err := MapProviderIDToResourceID(node.Spec.ProviderID)
	if err != nil {
		return "", errors.Wrap(err, "failed to map provider ID to instance ID")
	}

	// Get compartment ID for the node
	compartmentID, err := ic.cp.getCompartmentIDByInstanceID(instanceID)
	if err != nil {
		return "", errors.Wrap(err, "failed to get compartment ID")
	}

	podVNIC, err := ic.getPodVNICForNode(ctx, instanceID, compartmentID, node)
	if err != nil || podVNIC == nil {
		return "", errors.Wrap(err, "failed to get pod VNIC for node")
	}

	privateIP, err := ic.cp.client.Networking(nil).CreatePrivateIp(ctx, *podVNIC.Id, ic.cp.config.IPAM.NodeCIDRMaskSizeIPv4)
	if err != nil || privateIP.Ipv4SubnetCidrAtCreation == nil {
		return "", errors.Wrap(err, "failed to associate CIDR to node secondary VNIC")
	}

	if err = ic.updateNodePodCIDR(ctx, node, *privateIP.Ipv4SubnetCidrAtCreation); err != nil {
		return "", errors.Wrap(err, "failed to update node pod CIDR for node secondary VNIC")
	}

	return *privateIP.Ipv4SubnetCidrAtCreation, nil
}

// updateNodePodCIDR updates the node's Spec.PodCIDR field and sets the NodeNetworkUnavailable condition to False.
func (ic *IPAMController) updateNodePodCIDR(ctx context.Context, node *v1.Node, podCIDR string) error {
	// Create a patch to update the node
	patch := fmt.Sprintf(`{"spec":{"podCIDR":"%s","podCIDRs":["%s"]}}`, podCIDR, podCIDR)

	_, err := ic.cp.kubeclient.CoreV1().Nodes().Patch(
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
	if err := ic.updateNodeNetworkCondition(ctx, node); err != nil {
		ic.logger.With("node", node.Name, "error", err).Warn("Failed to update node network condition")
		// Don't return error here as the CIDR was successfully assigned
	}

	return nil
}

// updateNodeNetworkCondition sets the NodeNetworkUnavailable condition to False.
func (ic *IPAMController) updateNodeNetworkCondition(ctx context.Context, node *v1.Node) error {
	// Get the latest version of the node
	currentNode, err := ic.cp.kubeclient.CoreV1().Nodes().Get(ctx, node.Name, metav1.GetOptions{})
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

		_, err = ic.cp.kubeclient.CoreV1().Nodes().PatchStatus(ctx, node.Name, []byte(patch))
	} else {
		// Append new condition
		currentNode.Status.Conditions = append(currentNode.Status.Conditions, condition)
		_, err = ic.cp.kubeclient.CoreV1().Nodes().UpdateStatus(ctx, currentNode, metav1.UpdateOptions{})
	}

	return err
}

// enqueueNode adds a node to the workqueue.
func (ic *IPAMController) enqueueNode(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		ic.logger.With("error", err).Error("Failed to get key for node")
		return
	}
	ic.workqueue.Add(key)
}

// updateNode handles node update events.
func (ic *IPAMController) updateNode(old, new interface{}) {
	oldNode := old.(*v1.Node)
	newNode := new.(*v1.Node)

	// Only process if the node doesn't have a pod CIDR yet
	if newNode.Spec.PodCIDR == "" && oldNode.Spec.PodCIDR == "" {
		ic.enqueueNode(new)
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

// getPodVNICForNode finds the secondary VNIC in a pod subnet for the given node.
// Returns nil if no pod VNIC is found.
func (ic *IPAMController) getPodVNICForNode(ctx context.Context, instanceID, compartmentID string, node *v1.Node) (*core.Vnic, error) {
	// Get availability domain from node labels (set by kubelet/cloud provider)
	// Standard Kubernetes topology label
	availabilityDomain, ok := node.Labels["topology.kubernetes.io/zone"]
	if !ok {
		// Fall back to deprecated label
		availabilityDomain, ok = node.Labels["failure-domain.beta.kubernetes.io/zone"]
		if !ok {
			return nil, errors.New("node does not have availability domain label (topology.kubernetes.io/zone)")
		}
	}

	// Get the pod subnet ID for this availability domain
	// Try exact match first
	podSubnetID, ok := ic.cp.config.IPAM.PodSubnetIDs[availabilityDomain]
	if !ok {
		// If not found, try with realm prefix (format: "realm:AD-NAME")
		// This handles cases where config uses full format but label has short format
		for configAD, configSubnetID := range ic.cp.config.IPAM.PodSubnetIDs {
			// Check if the config AD ends with the node's AD (after the colon)
			if parts := strings.Split(configAD, ":"); len(parts) == 2 && parts[1] == availabilityDomain {
				podSubnetID = configSubnetID
				ok = true
				break
			}
		}
		if !ok {
			// No pod subnet configured for this AD
			return nil, errors.Errorf("no pod subnet configured for availability domain %s", availabilityDomain)
		}
	}

	// Get secondary VNICs for the instance
	secondaryVNICs, err := ic.cp.client.Compute().GetSecondaryVNICsForInstance(ctx, compartmentID, instanceID)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get secondary VNICs")
	}

	// Look for a VNIC in the pod subnet
	for _, vnic := range secondaryVNICs {
		if vnic.SubnetId != nil && *vnic.SubnetId == podSubnetID {
			return vnic, nil
		}
	}

	// No pod VNIC found
	return nil, nil
}
