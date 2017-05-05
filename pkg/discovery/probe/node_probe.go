package probe

import (
	"fmt"
	"strconv"
	"time"

	"k8s.io/kubernetes/pkg/api"
	client "k8s.io/kubernetes/pkg/client/unversioned"
	"k8s.io/kubernetes/pkg/fields"
	"k8s.io/kubernetes/pkg/labels"

	vmtAdvisor "github.com/turbonomic/kubeturbo/pkg/cadvisor"
	"github.com/turbonomic/kubeturbo/pkg/discovery/probe/stitching"

	"github.com/turbonomic/turbo-go-sdk/pkg/builder"
	"github.com/turbonomic/turbo-go-sdk/pkg/proto"

	"github.com/golang/glog"
	cadvisor "github.com/google/cadvisor/info/v1"
)

// key: node name; value: cAdvisor host.
var hostSet map[string]*vmtAdvisor.Host = make(map[string]*vmtAdvisor.Host)

// key: node name; value: node uid.
var nodeUIDMap map[string]string = make(map[string]string)

// A map stores the machine information of each node.
// key: node name; value: machine information.
var nodeMachineInfoMap map[string]*cadvisor.MachineInfo = make(map[string]*cadvisor.MachineInfo)

// This set keeps track of unschedulable or inactive nodes, which are not monitored in Turbonomic.
// key: node name.
var notMonitoredNodes map[string]struct{}

type NodeProbe struct {
	nodesGetter      NodesGetter
	config           *ProbeConfig
	stitchingManager *stitching.StitchingManager

	nodeIPMap map[string]map[api.NodeAddressType]string
}

// Since this is only used in probe package, do not expose it.
func NewNodeProbe(getter NodesGetter, config *ProbeConfig, stitchingManager *stitching.StitchingManager) *NodeProbe {
	// initiate set every time.
	notMonitoredNodes = make(map[string]struct{})

	return &NodeProbe{
		nodesGetter:      getter,
		config:           config,
		stitchingManager: stitchingManager,
	}
}

type VMTNodeGetter struct {
	kubeClient *client.Client
}

func NewVMTNodeGetter(kubeClient *client.Client) *VMTNodeGetter {
	return &VMTNodeGetter{
		kubeClient: kubeClient,
	}
}

// Get all nodes
func (this *VMTNodeGetter) GetNodes(label labels.Selector, field fields.Selector) ([]*api.Node, error) {
	listOption := &api.ListOptions{
		LabelSelector: label,
		FieldSelector: field,
	}
	nodeList, err := this.kubeClient.Nodes().List(*listOption)
	if err != nil {
		return nil, fmt.Errorf("Failed to get nodes from Kubernetes cluster: %s", err)
	}
	var nodeItems []*api.Node
	for _, node := range nodeList.Items {
		n := node
		nodeItems = append(nodeItems, &n)
	}
	glog.V(2).Infof("Discovering Nodes.. The cluster has " + strconv.Itoa(len(nodeItems)) + " nodes")
	return nodeItems, nil
}

type NodesGetter func(label labels.Selector, field fields.Selector) ([]*api.Node, error)

func (nodeProbe *NodeProbe) GetNodes(label labels.Selector, field fields.Selector) ([]*api.Node, error) {
	return nodeProbe.nodesGetter(label, field)
}

// Parse each node inside K8s. Get the resources usage of each node and build the entityDTO.
func (nodeProbe *NodeProbe) parseNodeFromK8s(nodes []*api.Node, pods []*api.Pod) (result []*proto.EntityDTO, err error) {
	nodePodsMap := make(map[string][]*api.Pod)
	for _, pod := range pods {
		var podList []*api.Pod
		if l, exist := nodePodsMap[pod.Spec.NodeName]; exist {
			podList = l
		}
		podList = append(podList, pod)
		nodePodsMap[pod.Spec.NodeName] = podList
	}
	for _, node := range nodes {

		// use cAdvisor to get node info
		nodeProbe.parseNodeIP(node)
		hostSet[node.Name] = nodeProbe.getHost(node.Name)

		// find stitching values.
		nodeProbe.stitchingManager.StoreStitchingValue(node)

		commoditiesSold, err := nodeProbe.createCommoditySold(node, nodePodsMap)
		if err != nil {
			glog.Errorf("Error when create commoditiesSold for %s: %s", node.Name, err)
			continue
		}
		entityDto, err := nodeProbe.buildVMEntityDTO(node, commoditiesSold)
		if err != nil {
			glog.Errorf("Error: %s", err)
			// failed to build entityDTO for the current node. Skip.
			continue
		}
		result = append(result, entityDto)
	}

	for _, entityDto := range result {
		glog.V(4).Infof("Node EntityDTO: " + entityDto.GetDisplayName())
		for _, c := range entityDto.CommoditiesSold {
			glog.V(5).Infof("Node commodity type is " + strconv.Itoa(int(c.GetCommodityType())) + "\n")
		}
	}

	return
}

func (nodeProbe *NodeProbe) createCommoditySold(node *api.Node, nodePodsMap map[string][]*api.Pod) ([]*proto.CommodityDTO, error) {
	var commoditiesSold []*proto.CommodityDTO
	nodeResourceStat, err := nodeProbe.getNodeResourceStat(node, nodePodsMap)
	if err != nil {
		return commoditiesSold, err
	}
	nodeID := string(node.UID)
	vMemComm, err := builder.NewCommodityDTOBuilder(proto.CommodityDTO_VMEM).
		Capacity(nodeResourceStat.vMemCapacity).
		Used(nodeResourceStat.vMemUsed).
		Create()
	if err != nil {
		return nil, err
	}
	commoditiesSold = append(commoditiesSold, vMemComm)

	vCpuComm, err := builder.NewCommodityDTOBuilder(proto.CommodityDTO_VCPU).
		Capacity(float64(nodeResourceStat.vCpuCapacity)).
		Used(nodeResourceStat.vCpuUsed).
		Create()
	if err != nil {
		return nil, err
	}
	commoditiesSold = append(commoditiesSold, vCpuComm)

	memProvisionedComm, err := builder.NewCommodityDTOBuilder(proto.CommodityDTO_MEM_PROVISIONED).
		//		Key("Container").
		Capacity(float64(nodeResourceStat.memProvisionedCapacity)).
		Used(nodeResourceStat.memProvisionedUsed).
		Create()
	if err != nil {
		return nil, err
	}
	commoditiesSold = append(commoditiesSold, memProvisionedComm)

	cpuProvisionedComm, err := builder.NewCommodityDTOBuilder(proto.CommodityDTO_CPU_PROVISIONED).
		//		Key("Container").
		Capacity(float64(nodeResourceStat.cpuProvisionedCapacity)).
		Used(nodeResourceStat.cpuProvisionedUsed).
		Create()
	if err != nil {
		return nil, err
	}
	commoditiesSold = append(commoditiesSold, cpuProvisionedComm)

	appComm, err := builder.NewCommodityDTOBuilder(proto.CommodityDTO_APPLICATION).
		Key(nodeID).
		Capacity(1E10).
		Create()
	if err != nil {
		return nil, err
	}
	commoditiesSold = append(commoditiesSold, appComm)

	labelsmap := node.ObjectMeta.Labels
	if len(labelsmap) > 0 {
		for key, value := range labelsmap {
			label := key + "=" + value
			glog.V(4).Infof("label for this Node is : %s", label)
			accessComm, err := builder.NewCommodityDTOBuilder(proto.CommodityDTO_VMPM_ACCESS).
				Key(label).
				Capacity(1E10).
				Create()
			if err != nil {
				return nil, err
			}
			commoditiesSold = append(commoditiesSold, accessComm)
		}
	}

	// Use Kubernetes service UID as the key for cluster commodity
	clusterCommodityKey := ClusterID
	clusterComm, err := builder.NewCommodityDTOBuilder(proto.CommodityDTO_CLUSTER).
		Key(clusterCommodityKey).
		Capacity(1E10).
		Create()
	if err != nil {
		return nil, err
	}
	commoditiesSold = append(commoditiesSold, clusterComm)

	return commoditiesSold, nil
}

// Get the cAdvisor host endpoint associated with node based on the given node name.
func (nodeProbe *NodeProbe) getHost(nodeName string) *vmtAdvisor.Host {
	nodeIP, err := nodeProbe.getNodeIPWithType(nodeName, api.NodeLegacyHostIP)
	if err != nil {
		glog.Errorf("Error getting NodeLegacyHostIP of node %s: %s.", nodeName, err)
		return nil
	}
	// Use NodeLegacyHostIP to build the host to interact with cAdvisor.
	host := &vmtAdvisor.Host{
		IP:       nodeIP,
		Port:     nodeProbe.config.CadvisorPort,
		Resource: "",
	}
	return host
}

// Retrieve the legacyHostIP of each node and put other IPs to related maps.
func (nodeProbe *NodeProbe) parseNodeIP(node *api.Node) {
	if nodeProbe.nodeIPMap == nil {
		nodeProbe.nodeIPMap = make(map[string]map[api.NodeAddressType]string)
	}
	currentNodeIPMap := make(map[api.NodeAddressType]string)

	nodeAddresses := node.Status.Addresses
	for _, nodeAddress := range nodeAddresses {
		currentNodeIPMap[nodeAddress.Type] = nodeAddress.Address
	}

	nodeProbe.nodeIPMap[node.Name] = currentNodeIPMap
}

// Retrieve all available IP of a node and store them in a map according to IP address type.
func (nodeProbe *NodeProbe) getNodeIPWithType(nodeName string, ipType api.NodeAddressType) (string, error) {
	ips, ok := nodeProbe.nodeIPMap[nodeName]
	if !ok {
		return "", fmt.Errorf("IP of node %s is not tracked", nodeName)
	}
	nodeIP, exist := ips[ipType]
	if !exist {
		return "", fmt.Errorf("node %s does not have %s.", nodeName, ipType)
	}
	return nodeIP, nil
}

// Build entityDTO for a single node.
func (nodeProbe *NodeProbe) buildVMEntityDTO(node *api.Node, commoditiesSold []*proto.CommodityDTO) (*proto.EntityDTO, error) {
	// Now start to build supply chain.
	nodeUID := string(node.UID)
	displayName := node.Name
	nodeUIDMap[node.Name] = nodeUID

	entityDTOBuilder := builder.NewEntityDTOBuilder(proto.EntityDTO_VIRTUAL_MACHINE, nodeUID)
	entityDTOBuilder.DisplayName(displayName)

	// sells commodities
	entityDTOBuilder.SellsCommodities(commoditiesSold)

	// property
	property, err := nodeProbe.stitchingManager.BuildStitchingProperty(node.Name, stitching.Reconcile)
	if err != nil {
		return nil, fmt.Errorf("Failed to build EntityDTO for node %s: %s", node.Name, err)
	}
	entityDTOBuilder = entityDTOBuilder.WithProperty(property)
	glog.V(4).Infof("Node %s will be reconciled with VM with %s: %s", node.Name, *property.Name, *property.Value)

	// reconciliation meta data
	metaData, err := nodeProbe.stitchingManager.GenerateReconciliationMetaData()
	if err != nil {
		return nil, fmt.Errorf("Failed to build EntityDTO for node %s: %s", node.Name, err)
	}
	entityDTOBuilder = entityDTOBuilder.ReplacedBy(metaData)

	// power state
	entityDTOBuilder = entityDTOBuilder.WithPowerState(proto.EntityDTO_POWERED_ON)

	// We do not monitor node that is not ready or unschedulable.
	if !nodeIsReady(node) || !nodeIsSchedulable(node) {
		glog.V(3).Infof("Node %s is either not ready or unschedulable. Skip", node.Name)
		//continue
		notMonitoredNodes[node.Name] = struct{}{}
		entityDTOBuilder.Monitored(false)
	}

	entityDto, err := entityDTOBuilder.Create()
	if err != nil {
		return nil, fmt.Errorf("Failed to build EntityDTO for node %s: %s", nodeUID, err)
	}

	return entityDto, nil
}

// Get current stat of node resources, such as capacity and used values.
func (nodeProbe *NodeProbe) getNodeResourceStat(node *api.Node, nodePodsMap map[string][]*api.Pod) (*NodeResourceStat, error) {
	source := &vmtAdvisor.CadvisorSource{}

	host := nodeProbe.getHost(node.Name)
	machineInfo, err := source.GetMachineInfo(*host)
	if err != nil {
		return nil, fmt.Errorf("Error getting machineInfo for %s: %s", node.Name, err)
	}
	// The return cpu frequency is in KHz, we need MHz
	cpuFrequency := machineInfo.CpuFrequency / 1000
	nodeMachineInfoMap[node.Name] = machineInfo

	// Here we only need the root container.
	_, root, err := source.GetAllContainers(*host, time.Now(), time.Now())
	if err != nil {
		return nil, fmt.Errorf("Error getting root container info for %s: %s", node.Name, err)
	}
	containerStats := root.Stats
	// To get a valid cpu usage, there must be at least 2 valid stats.
	if len(containerStats) < 2 {
		glog.Warning("Not enough data")
		return nil, fmt.Errorf("Not enough status data of current node %s.", node.Name)
	}
	currentStat := containerStats[len(containerStats)-1]
	prevStat := containerStats[len(containerStats)-2]
	rawUsage := int64(currentStat.Cpu.Usage.Total - prevStat.Cpu.Usage.Total)
	glog.V(4).Infof("rawUsage is %d", rawUsage)
	intervalInNs := currentStat.Timestamp.Sub(prevStat.Timestamp).Nanoseconds()
	glog.V(4).Infof("interval is %d", intervalInNs)
	rootCurCpu := float64(rawUsage) * 1.0 / float64(intervalInNs)
	rootCurMem := float64(currentStat.Memory.Usage) / 1024 // Mem is returned in B

	// Get the node Cpu and Mem capacity.
	nodeCpuCapacity := float64(machineInfo.NumCores) * float64(cpuFrequency)
	nodeMemCapacity := float64(machineInfo.MemoryCapacity) / 1024 // Mem is returned in B
	glog.V(3).Infof("Discovered node is " + node.Name)
	glog.V(4).Infof("Node CPU capacity is %f", nodeCpuCapacity)
	glog.V(4).Infof("Node Mem capacity is %f", nodeMemCapacity)
	// Find out the used value for each commodity
	cpuUsed := float64(rootCurCpu) * float64(cpuFrequency)
	memUsed := float64(rootCurMem)

	cpuProvisionedUsed := float64(0)
	memProvisionedUsed := float64(0)

	if podList, exist := nodePodsMap[node.Name]; exist {
		for _, pod := range podList {
			cpuRequest, memRequest, err := GetResourceRequest(pod)
			if err != nil {
				return nil, fmt.Errorf("Error getting provisioned resource consumption: %s", err)
			}
			cpuProvisionedUsed += cpuRequest
			memProvisionedUsed += memRequest
		}
	}
	cpuProvisionedUsed *= float64(cpuFrequency)

	return &NodeResourceStat{
		vCpuCapacity:           nodeCpuCapacity,
		vCpuUsed:               cpuUsed,
		vMemCapacity:           nodeMemCapacity,
		vMemUsed:               memUsed,
		cpuProvisionedCapacity: nodeCpuCapacity,
		cpuProvisionedUsed:     cpuProvisionedUsed,
		memProvisionedCapacity: nodeMemCapacity,
		memProvisionedUsed:     memProvisionedUsed,
	}, nil
}