/*
Copyright 2019 The Kubernetes Authors.

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

package packet

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"sync"

	apiv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/autoscaler/cluster-autoscaler/cloudprovider"
	"k8s.io/autoscaler/cluster-autoscaler/config"
	"k8s.io/autoscaler/cluster-autoscaler/config/dynamic"
	"k8s.io/autoscaler/cluster-autoscaler/utils/errors"
	klog "k8s.io/klog/v2"
)

const (
	// ProviderName is the cloud provider name for Packet
	ProviderName = "packet"
	// GPULabel is the label added to nodes with GPU resource.
	GPULabel = "cloud.google.com/gke-accelerator"
	// DefaultControllerNodeLabelKey is the label added to Master/Controller to identify as
	// master/controller node.
	DefaultControllerNodeLabelKey = "node-role.kubernetes.io/master"
	// ControllerNodeIdentifierEnv is the string for the environment variable.
	ControllerNodeIdentifierEnv = "PACKET_CONTROLLER_NODE_IDENTIFIER_LABEL"
)

var (
	availableGPUTypes = map[string]struct{}{
		"nvidia-tesla-v100": {},
	}
)

// packetCloudProvider implements CloudProvider interface from cluster-autoscaler/cloudprovider module.
type packetCloudProvider struct {
	packetManager   packetManager
	resourceLimiter *cloudprovider.ResourceLimiter
	nodeGroups      []packetNodeGroup
}

func buildPacketCloudProvider(packetManager packetManager, resourceLimiter *cloudprovider.ResourceLimiter) (cloudprovider.CloudProvider, error) {
	pcp := &packetCloudProvider{
		packetManager:   packetManager,
		resourceLimiter: resourceLimiter,
		nodeGroups:      []packetNodeGroup{},
	}
	return pcp, nil
}

// Name returns the name of the cloud provider.
func (pcp *packetCloudProvider) Name() string {
	return ProviderName
}

// GPULabel returns the label added to nodes with GPU resource.
func (pcp *packetCloudProvider) GPULabel() string {
	return GPULabel
}

// GetAvailableGPUTypes return all available GPU types cloud provider supports
func (pcp *packetCloudProvider) GetAvailableGPUTypes() map[string]struct{} {
	return availableGPUTypes
}

// NodeGroups returns all node groups managed by this cloud provider.
func (pcp *packetCloudProvider) NodeGroups() []cloudprovider.NodeGroup {
	groups := make([]cloudprovider.NodeGroup, len(pcp.nodeGroups))
	for i := range pcp.nodeGroups {
		groups[i] = &pcp.nodeGroups[i]
	}
	return groups
}

// AddNodeGroup appends a node group to the list of node groups managed by this cloud provider.
func (pcp *packetCloudProvider) AddNodeGroup(group packetNodeGroup) {
	pcp.nodeGroups = append(pcp.nodeGroups, group)
}

// NodeGroupForNode returns the node group that a given node belongs to.
//
// Since only a single node group is currently supported, the first node group is always returned.
func (pcp *packetCloudProvider) NodeGroupForNode(node *apiv1.Node) (cloudprovider.NodeGroup, error) {
	controllerNodeLabel := os.Getenv(ControllerNodeIdentifierEnv)
	if controllerNodeLabel == "" {
		klog.V(3).Infof("env %s not set, using default: %s", ControllerNodeIdentifierEnv, DefaultControllerNodeLabelKey)
		controllerNodeLabel = DefaultControllerNodeLabelKey
	}

	if _, found := node.ObjectMeta.Labels[controllerNodeLabel]; found {
		return nil, nil
	}
	nodeGroupId, err := pcp.packetManager.NodeGroupForNode(node.ObjectMeta.Labels, node.Spec.ProviderID)
	if err != nil {
		return nil, err
	}
	for i, nodeGroup := range pcp.nodeGroups {
		if nodeGroup.Id() == nodeGroupId {
			return &(pcp.nodeGroups[i]), nil
		}
	}
	return nil, fmt.Errorf("Could not find group for node: %s", node.Spec.ProviderID)
}

// HasInstance returns whether a given node has a corresponding instance in this cloud provider
func (pcp *packetCloudProvider) HasInstance(node *apiv1.Node) (bool, error) {
	return true, cloudprovider.ErrNotImplemented
}

// Pricing returns pricing model for this cloud provider or error if not available.
func (pcp *packetCloudProvider) Pricing() (cloudprovider.PricingModel, errors.AutoscalerError) {
	return &PacketPriceModel{}, nil
}

// GetAvailableMachineTypes is not implemented.
func (pcp *packetCloudProvider) GetAvailableMachineTypes() ([]string, error) {
	return []string{}, nil
}

// NewNodeGroup is not implemented.
func (pcp *packetCloudProvider) NewNodeGroup(machineType string, labels map[string]string, systemLabels map[string]string,
	taints []apiv1.Taint, extraResources map[string]resource.Quantity) (cloudprovider.NodeGroup, error) {
	return nil, cloudprovider.ErrNotImplemented
}

// GetResourceLimiter returns resource constraints for the cloud provider
func (pcp *packetCloudProvider) GetResourceLimiter() (*cloudprovider.ResourceLimiter, error) {
	return pcp.resourceLimiter, nil
}

// Refresh is called before every autoscaler main loop.
//
// Currently only prints debug information.
func (pcp *packetCloudProvider) Refresh() error {
	for _, nodegroup := range pcp.nodeGroups {
		klog.V(3).Info(nodegroup.Debug())
	}
	return nil
}

// Cleanup currently does nothing.
func (pcp *packetCloudProvider) Cleanup() error {
	return nil
}

// BuildPacket is called by the autoscaler to build a packet cloud provider.
//
// The packetManager is created here, and the node groups are created
// based on the specs provided via the command line parameters.
func BuildPacket(opts config.AutoscalingOptions, do cloudprovider.NodeGroupDiscoveryOptions, rl *cloudprovider.ResourceLimiter) cloudprovider.CloudProvider {
	var config io.ReadCloser

	if opts.CloudConfig != "" {
		var err error
		config, err = os.Open(opts.CloudConfig)
		if err != nil {
			klog.Fatalf("Couldn't open cloud provider configuration %s: %#v", opts.CloudConfig, err)
		}
		defer config.Close()
	}

	manager, err := createPacketManager(config, do, opts)
	if err != nil {
		klog.Fatalf("Failed to create packet manager: %v", err)
	}

	provider, err := buildPacketCloudProvider(manager, rl)
	if err != nil {
		klog.Fatalf("Failed to create packet cloud provider: %v", err)
	}

	if len(do.NodeGroupSpecs) == 0 {
		klog.Fatalf("Must specify at least one node group with --nodes=<min>:<max>:<name>,...")
	}

	validNodepoolName := regexp.MustCompile(`^[a-z0-9A-Z]+[a-z0-9A-Z\-\.\_]*[a-z0-9A-Z]+$|^[a-z0-9A-Z]{1}$`)

	clusterUpdateLock := sync.Mutex{}

	for _, nodegroupSpec := range do.NodeGroupSpecs {
		spec, err := dynamic.SpecFromString(nodegroupSpec, scaleToZeroSupported)
		if err != nil {
			klog.Fatalf("Could not parse node group spec %s: %v", nodegroupSpec, err)
		}

		if !validNodepoolName.MatchString(spec.Name) || len(spec.Name) > 63 {
			klog.Fatalf("Invalid nodepool name: %s\nMust be a valid kubernetes label value", spec.Name)
		}

		ng := packetNodeGroup{
			packetManager:       manager,
			id:                  spec.Name,
			clusterUpdateMutex:  &clusterUpdateLock,
			minSize:             spec.MinSize,
			maxSize:             spec.MaxSize,
			targetSize:          new(int),
			waitTimeStep:        waitForStatusTimeStep,
			deleteBatchingDelay: deleteNodesBatchingDelay,
		}
		*ng.targetSize, err = ng.packetManager.nodeGroupSize(ng.id)
		if err != nil {
			klog.Fatalf("Could not set current nodes in node group: %v", err)
		}
		provider.(*packetCloudProvider).AddNodeGroup(ng)
	}

	return provider
}
