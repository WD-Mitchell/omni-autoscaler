package controller

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/WD-Mitchell/omni-autoscaler/internal/config"
	"github.com/WD-Mitchell/omni-autoscaler/internal/omni"
)

// Autoscaler manages the scaling of Omni machine sets based on cluster load
type Autoscaler struct {
	kubeClient  kubernetes.Interface
	omniClient  *omni.Client
	config      *config.Config
	logger      *slog.Logger

	// Track last scaling times for cooldowns
	mu              sync.Mutex
	lastScaleUp     map[string]time.Time
	lastScaleDown   map[string]time.Time

	// Track nodes being drained for safe scale-down
	drainingNodes   map[string]time.Time // nodeName -> drain start time
}

// NewAutoscaler creates a new Autoscaler instance
func NewAutoscaler(kubeClient kubernetes.Interface, omniClient *omni.Client, cfg *config.Config, logger *slog.Logger) *Autoscaler {
	return &Autoscaler{
		kubeClient:    kubeClient,
		omniClient:    omniClient,
		config:        cfg,
		logger:        logger,
		lastScaleUp:   make(map[string]time.Time),
		lastScaleDown: make(map[string]time.Time),
		drainingNodes: make(map[string]time.Time),
	}
}

// Run starts the autoscaler reconciliation loop
func (a *Autoscaler) Run(ctx context.Context) error {
	a.logger.Info("Starting autoscaler", "syncInterval", a.config.SyncInterval)

	ticker := time.NewTicker(a.config.SyncInterval.Duration())
	defer ticker.Stop()

	// Run immediately on start
	if err := a.reconcile(ctx); err != nil {
		a.logger.Error("Reconciliation failed", "error", err)
	}

	for {
		select {
		case <-ctx.Done():
			a.logger.Info("Autoscaler stopped")
			return ctx.Err()
		case <-ticker.C:
			if err := a.reconcile(ctx); err != nil {
				a.logger.Error("Reconciliation failed", "error", err)
			}
		}
	}
}

// reconcile performs a single reconciliation cycle
func (a *Autoscaler) reconcile(ctx context.Context) error {
	a.logger.Info("Starting reconciliation cycle")

	// Check if upgrades are in progress and we should pause scaling
	if a.config.DisableDuringUpgrades {
		upgradeInProgress, upgradeType, err := a.omniClient.IsUpgradeInProgress(ctx)
		if err != nil {
			a.logger.Warn("Failed to check upgrade status", "error", err)
			// Continue with scaling on error to avoid blocking autoscaling completely
		} else if upgradeInProgress {
			a.logger.Info("Cluster upgrade in progress, skipping autoscaling to prevent interference",
				"upgrade_type", upgradeType)
			return nil
		}
	}

	// Get pending pods
	pendingPods, err := a.getPendingPods(ctx)
	if err != nil {
		return fmt.Errorf("failed to get pending pods: %w", err)
	}

	// Get cluster resource usage
	resources, err := a.getClusterResources(ctx)
	if err != nil {
		return fmt.Errorf("failed to get cluster resources: %w", err)
	}

	a.logger.Debug("Cluster status",
		"workerNodes", resources.WorkerCount,
		"cpuUtil", fmt.Sprintf("%.1f%%", resources.CPUUtilization*100),
		"memUtil", fmt.Sprintf("%.1f%%", resources.MemUtilization*100),
		"pendingPods", len(pendingPods),
	)

	// Reconcile each configured machine set
	for _, msConfig := range a.config.MachineSets {
		if err := a.reconcileMachineSet(ctx, msConfig, pendingPods, resources); err != nil {
			a.logger.Error("Failed to reconcile machine set", "machineSet", msConfig.Name, "error", err)
		}
	}

	return nil
}

// reconcileMachineSet handles scaling decisions for a single machine set
func (a *Autoscaler) reconcileMachineSet(ctx context.Context, msConfig config.MachineSetConfig, pendingPods []corev1.Pod, resources *ClusterResources) error {
	// Get current machine set status
	status, err := a.omniClient.GetMachineSetStatus(ctx, msConfig.Name)
	if err != nil {
		return fmt.Errorf("failed to get machine set status: %w", err)
	}

	currentCount := status.MachineCount
	desiredCount := currentCount
	pendingPodCount := len(pendingPods)

	a.logger.Debug("Evaluating machine set",
		"machineSet", msConfig.Name,
		"currentCount", currentCount,
		"pendingPods", pendingPodCount,
		"cpuUtil", fmt.Sprintf("%.1f%%", resources.CPUUtilization*100),
		"memUtil", fmt.Sprintf("%.1f%%", resources.MemUtilization*100),
		"scaleUpThreshold", fmt.Sprintf("%.1f%%", msConfig.ScaleUpThreshold*100),
		"scaleDownThreshold", fmt.Sprintf("%.1f%%", msConfig.ScaleDownThreshold*100),
	)

	// === SCALE UP LOGIC ===
	// Scale up when:
	// 1. There are pending pods (reactive)
	// 2. Cluster utilization exceeds threshold (proactive)
	shouldScaleUp := false
	scaleUpReason := ""

	// Check for pending pods (highest priority)
	if pendingPodCount >= msConfig.ScaleUpPendingPods && currentCount < msConfig.MaxSize {
		shouldScaleUp = true
		scaleUpReason = fmt.Sprintf("%d pending pods", pendingPodCount)
	}

	// Check for high utilization (proactive scaling)
	if !shouldScaleUp && resources.MaxUtilization >= msConfig.ScaleUpThreshold && currentCount < msConfig.MaxSize {
		shouldScaleUp = true
		scaleUpReason = fmt.Sprintf("utilization %.1f%% exceeds threshold %.1f%% (cpu=%.1f%%, mem=%.1f%%)",
			resources.MaxUtilization*100, msConfig.ScaleUpThreshold*100,
			resources.CPUUtilization*100, resources.MemUtilization*100)
	}

	if shouldScaleUp {
		if a.canScaleUp(msConfig.Name) {
			// Calculate how many nodes to add
			increment := 1
			if pendingPodCount > 0 {
				// Scale up based on pending pods
				increment = max(1, pendingPodCount/msConfig.ScaleUpPendingPods)
			} else {
				// Proactive scaling: add enough to get below target utilization
				// Estimate: if we're at X% util with N nodes, we need N * X / target nodes
				if resources.MaxUtilization > msConfig.TargetUtilization && resources.WorkerCount > 0 {
					targetNodes := int(float64(resources.WorkerCount) * resources.MaxUtilization / msConfig.TargetUtilization)
					increment = max(1, targetNodes-resources.WorkerCount)
				}
			}
			increment = min(increment, msConfig.MaxSize-currentCount)
			desiredCount = currentCount + increment

			a.logger.Info("Scaling up",
				"machineSet", msConfig.Name,
				"from", currentCount,
				"to", desiredCount,
				"reason", scaleUpReason,
			)
		} else {
			a.logger.Debug("Scale-up on cooldown", "machineSet", msConfig.Name)
		}
	}

	// === SCALE DOWN LOGIC ===
	// Scale down when:
	// 1. Utilization is below threshold
	// 2. No pending pods
	// 3. We can safely consolidate (pods would fit on remaining nodes)
	// 4. Node is drained and Longhorn volumes are healthy
	if desiredCount == currentCount && currentCount > msConfig.MinSize && pendingPodCount == 0 {
		if resources.MaxUtilization < msConfig.ScaleDownThreshold {
			if a.canScaleDown(msConfig.Name) {
				// Find the least utilized worker node
				leastUtilized := a.getLeastUtilizedWorker(resources)
				if leastUtilized != nil {
					// Check if we can safely remove this node
					if a.canConsolidateNode(resources, leastUtilized.Name, msConfig.SafeToEvictBuffer) {
						// Check if node is already being drained
						a.mu.Lock()
						drainStartTime, isDraining := a.drainingNodes[leastUtilized.Name]
						a.mu.Unlock()

						if !isDraining {
							// Start the drain process
							a.logger.Info("Starting safe scale-down - cordoning and draining node",
								"machineSet", msConfig.Name,
								"targetNode", leastUtilized.Name,
							)

							// Cordon the node first
							if err := a.cordonNode(ctx, leastUtilized.Name); err != nil {
								a.logger.Error("Failed to cordon node", "node", leastUtilized.Name, "error", err)
								return err
							}

							// Start draining
							if err := a.drainNode(ctx, leastUtilized.Name); err != nil {
								a.logger.Error("Failed to drain node", "node", leastUtilized.Name, "error", err)
								// Don't fail - we'll retry on the next reconcile
							}

							// Track that we're draining this node
							a.mu.Lock()
							a.drainingNodes[leastUtilized.Name] = time.Now()
							a.mu.Unlock()

							// Don't scale down yet - wait for drain to complete
							return nil
						}

						// Node is being drained - check if it's complete
						drained, err := a.isNodeDrained(ctx, leastUtilized.Name)
						if err != nil {
							a.logger.Error("Failed to check if node is drained", "node", leastUtilized.Name, "error", err)
							return err
						}

						if !drained {
							// Check for drain timeout (30 minutes)
							if time.Since(drainStartTime) > 30*time.Minute {
								a.logger.Warn("Node drain timeout - proceeding with scale-down anyway",
									"node", leastUtilized.Name,
									"drainDuration", time.Since(drainStartTime),
								)
							} else {
								a.logger.Debug("Waiting for node to drain",
									"node", leastUtilized.Name,
									"drainDuration", time.Since(drainStartTime),
								)
								return nil
							}
						}

						// Check Longhorn volume health before final scale-down
						volumesHealthy, err := a.checkLonghornVolumesHealthy(ctx)
						if err != nil {
							a.logger.Error("Failed to check Longhorn volume health", "error", err)
							return err
						}

						if !volumesHealthy {
							a.logger.Warn("Longhorn volumes not healthy - delaying scale-down",
								"node", leastUtilized.Name,
							)
							return nil
						}

						// All checks passed - proceed with scale-down
						desiredCount = currentCount - 1

						// Remove from draining tracking
						a.mu.Lock()
						delete(a.drainingNodes, leastUtilized.Name)
						a.mu.Unlock()

						a.logger.Info("Scaling down (node drained safely)",
							"machineSet", msConfig.Name,
							"from", currentCount,
							"to", desiredCount,
							"targetNode", leastUtilized.Name,
							"nodeUtil", fmt.Sprintf("%.1f%%", leastUtilized.MaxUtilization*100),
							"reason", fmt.Sprintf("cluster utilization %.1f%% below threshold %.1f%%, node %s drained and ready",
								resources.MaxUtilization*100, msConfig.ScaleDownThreshold*100, leastUtilized.Name),
						)
					} else {
						a.logger.Debug("Cannot scale down - consolidation would exceed capacity",
							"machineSet", msConfig.Name,
							"targetNode", leastUtilized.Name,
							"safetyBuffer", fmt.Sprintf("%.1f%%", msConfig.SafeToEvictBuffer*100),
						)
					}
				}
			} else {
				a.logger.Debug("Scale-down on cooldown", "machineSet", msConfig.Name)
			}
		}
	}

	// Apply scaling if needed
	if desiredCount != currentCount {
		if err := a.omniClient.ScaleMachineSet(ctx, msConfig.Name, desiredCount); err != nil {
			return fmt.Errorf("failed to scale machine set: %w", err)
		}

		// Update cooldown timestamps
		a.mu.Lock()
		if desiredCount > currentCount {
			a.lastScaleUp[msConfig.Name] = time.Now()
		} else {
			a.lastScaleDown[msConfig.Name] = time.Now()
		}
		a.mu.Unlock()
	}

	return nil
}

// getPendingPods returns pods that are pending due to insufficient resources
// Filters out ignored namespaces to avoid feedback loop from new node system pods
func (a *Autoscaler) getPendingPods(ctx context.Context) ([]corev1.Pod, error) {
	// Build ignored namespaces map from config
	ignoredNamespaces := make(map[string]bool)
	for _, ns := range a.config.IgnoredNamespaces {
		ignoredNamespaces[ns] = true
	}

	pods, err := a.kubeClient.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		FieldSelector: "status.phase=Pending",
	})
	if err != nil {
		return nil, err
	}

	var pendingPods []corev1.Pod
	for _, pod := range pods.Items {
		// Skip ignored namespaces
		if ignoredNamespaces[pod.Namespace] {
			continue
		}

		// Check if pod is unschedulable due to resources
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodScheduled && condition.Status == corev1.ConditionFalse {
				if condition.Reason == "Unschedulable" {
					pendingPods = append(pendingPods, pod)
					break
				}
			}
		}
	}

	return pendingPods, nil
}

// NodeResources tracks resource usage for a single node
type NodeResources struct {
	Name            string
	CPUAllocatable  int64 // millicores
	CPURequested    int64 // millicores
	MemAllocatable  int64 // bytes
	MemRequested    int64 // bytes
	CPUUtilization  float64
	MemUtilization  float64
	MaxUtilization  float64 // max of CPU and memory utilization
	PodCount        int
	IsControlPlane  bool
}

// ClusterResources aggregates resource information across the cluster
type ClusterResources struct {
	Nodes              []NodeResources
	TotalCPUAllocatable int64
	TotalCPURequested   int64
	TotalMemAllocatable int64
	TotalMemRequested   int64
	CPUUtilization      float64
	MemUtilization      float64
	MaxUtilization      float64 // max of CPU and memory
	WorkerCount         int
	// Per-node pod assignments for consolidation checks
	NodePods map[string][]corev1.Pod
}

// getClusterResources calculates detailed resource usage across the cluster
func (a *Autoscaler) getClusterResources(ctx context.Context) (*ClusterResources, error) {
	nodes, err := a.kubeClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list nodes: %w", err)
	}

	// Get all running pods
	pods, err := a.kubeClient.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		FieldSelector: "status.phase=Running",
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list pods: %w", err)
	}

	// Calculate requested resources per node and track pod assignments
	nodeCPURequests := make(map[string]int64)
	nodeMemRequests := make(map[string]int64)
	nodePodCounts := make(map[string]int)
	nodePods := make(map[string][]corev1.Pod)

	for _, pod := range pods.Items {
		if pod.Spec.NodeName == "" {
			continue
		}
		nodePodCounts[pod.Spec.NodeName]++
		nodePods[pod.Spec.NodeName] = append(nodePods[pod.Spec.NodeName], pod)

		for _, container := range pod.Spec.Containers {
			if cpu := container.Resources.Requests.Cpu(); cpu != nil {
				nodeCPURequests[pod.Spec.NodeName] += cpu.MilliValue()
			}
			if mem := container.Resources.Requests.Memory(); mem != nil {
				nodeMemRequests[pod.Spec.NodeName] += mem.Value()
			}
		}
	}

	resources := &ClusterResources{
		Nodes:    make([]NodeResources, 0, len(nodes.Items)),
		NodePods: nodePods,
	}

	for _, node := range nodes.Items {
		isControlPlane := false
		if _, ok := node.Labels["node-role.kubernetes.io/control-plane"]; ok {
			isControlPlane = true
		}

		cpuAllocatable := node.Status.Allocatable.Cpu().MilliValue()
		memAllocatable := node.Status.Allocatable.Memory().Value()
		cpuRequested := nodeCPURequests[node.Name]
		memRequested := nodeMemRequests[node.Name]

		var cpuUtil, memUtil float64
		if cpuAllocatable > 0 {
			cpuUtil = float64(cpuRequested) / float64(cpuAllocatable)
		}
		if memAllocatable > 0 {
			memUtil = float64(memRequested) / float64(memAllocatable)
		}
		maxUtil := cpuUtil
		if memUtil > maxUtil {
			maxUtil = memUtil
		}

		nodeRes := NodeResources{
			Name:            node.Name,
			CPUAllocatable:  cpuAllocatable,
			CPURequested:    cpuRequested,
			MemAllocatable:  memAllocatable,
			MemRequested:    memRequested,
			CPUUtilization:  cpuUtil,
			MemUtilization:  memUtil,
			MaxUtilization:  maxUtil,
			PodCount:        nodePodCounts[node.Name],
			IsControlPlane:  isControlPlane,
		}
		resources.Nodes = append(resources.Nodes, nodeRes)

		// Only count worker nodes for totals
		if !isControlPlane {
			resources.TotalCPUAllocatable += cpuAllocatable
			resources.TotalCPURequested += cpuRequested
			resources.TotalMemAllocatable += memAllocatable
			resources.TotalMemRequested += memRequested
			resources.WorkerCount++
		}
	}

	// Calculate cluster-wide utilization
	if resources.TotalCPUAllocatable > 0 {
		resources.CPUUtilization = float64(resources.TotalCPURequested) / float64(resources.TotalCPUAllocatable)
	}
	if resources.TotalMemAllocatable > 0 {
		resources.MemUtilization = float64(resources.TotalMemRequested) / float64(resources.TotalMemAllocatable)
	}
	resources.MaxUtilization = resources.CPUUtilization
	if resources.MemUtilization > resources.MaxUtilization {
		resources.MaxUtilization = resources.MemUtilization
	}

	return resources, nil
}

// getLeastUtilizedWorker returns the worker node with the lowest max utilization
func (a *Autoscaler) getLeastUtilizedWorker(resources *ClusterResources) *NodeResources {
	var leastUtilized *NodeResources
	for i := range resources.Nodes {
		node := &resources.Nodes[i]
		if node.IsControlPlane {
			continue
		}
		if leastUtilized == nil || node.MaxUtilization < leastUtilized.MaxUtilization {
			leastUtilized = node
		}
	}
	return leastUtilized
}

// canConsolidateNode checks if pods from a node can fit on remaining nodes
func (a *Autoscaler) canConsolidateNode(resources *ClusterResources, nodeToRemove string, safetyBuffer float64) bool {
	// Find the node to remove and its pods
	var nodeRes *NodeResources
	for i := range resources.Nodes {
		if resources.Nodes[i].Name == nodeToRemove {
			nodeRes = &resources.Nodes[i]
			break
		}
	}
	if nodeRes == nil {
		return false
	}

	// Calculate remaining capacity after removing this node
	remainingCPUAllocatable := resources.TotalCPUAllocatable - nodeRes.CPUAllocatable
	remainingMemAllocatable := resources.TotalMemAllocatable - nodeRes.MemAllocatable

	// Total requests stay the same (pods will move to other nodes)
	totalCPURequested := resources.TotalCPURequested
	totalMemRequested := resources.TotalMemRequested

	if remainingCPUAllocatable <= 0 || remainingMemAllocatable <= 0 {
		return false
	}

	// Check if pods would fit with safety buffer
	cpuUtilAfter := float64(totalCPURequested) / float64(remainingCPUAllocatable)
	memUtilAfter := float64(totalMemRequested) / float64(remainingMemAllocatable)
	maxUtilAfter := cpuUtilAfter
	if memUtilAfter > maxUtilAfter {
		maxUtilAfter = memUtilAfter
	}

	// Allow consolidation if resulting utilization is below (1 - safety buffer)
	maxAllowedUtil := 1.0 - safetyBuffer
	return maxUtilAfter <= maxAllowedUtil
}

// getNodeUtilization calculates the average CPU/memory utilization across all nodes (legacy compatibility)
func (a *Autoscaler) getNodeUtilization(ctx context.Context) (float64, error) {
	resources, err := a.getClusterResources(ctx)
	if err != nil {
		return 0, err
	}
	return resources.CPUUtilization, nil
}

// canScaleUp checks if scale-up is allowed based on cooldown
func (a *Autoscaler) canScaleUp(machineSet string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	lastScale, ok := a.lastScaleUp[machineSet]
	if !ok {
		return true
	}
	return time.Since(lastScale) >= a.config.Cooldowns.ScaleUp.Duration()
}

// canScaleDown checks if scale-down is allowed based on cooldown
func (a *Autoscaler) canScaleDown(machineSet string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	// Also check if we recently scaled up
	if lastUp, ok := a.lastScaleUp[machineSet]; ok {
		if time.Since(lastUp) < a.config.Cooldowns.ScaleDown.Duration() {
			return false
		}
	}

	lastScale, ok := a.lastScaleDown[machineSet]
	if !ok {
		return true
	}
	return time.Since(lastScale) >= a.config.Cooldowns.ScaleDown.Duration()
}

// cordonNode marks a node as unschedulable
func (a *Autoscaler) cordonNode(ctx context.Context, nodeName string) error {
	node, err := a.kubeClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get node %s: %w", nodeName, err)
	}

	if node.Spec.Unschedulable {
		a.logger.Debug("Node already cordoned", "node", nodeName)
		return nil
	}

	node.Spec.Unschedulable = true
	_, err = a.kubeClient.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to cordon node %s: %w", nodeName, err)
	}

	a.logger.Info("Cordoned node", "node", nodeName)
	return nil
}

// uncordonNode marks a node as schedulable again
func (a *Autoscaler) uncordonNode(ctx context.Context, nodeName string) error {
	node, err := a.kubeClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get node %s: %w", nodeName, err)
	}

	if !node.Spec.Unschedulable {
		return nil
	}

	node.Spec.Unschedulable = false
	_, err = a.kubeClient.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to uncordon node %s: %w", nodeName, err)
	}

	a.logger.Info("Uncordoned node", "node", nodeName)
	return nil
}

// evictPod evicts a single pod using the Eviction API
func (a *Autoscaler) evictPod(ctx context.Context, pod corev1.Pod) error {
	eviction := &policyv1.Eviction{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pod.Name,
			Namespace: pod.Namespace,
		},
		DeleteOptions: &metav1.DeleteOptions{
			GracePeriodSeconds: ptr(int64(30)),
		},
	}

	err := a.kubeClient.CoreV1().Pods(pod.Namespace).EvictV1(ctx, eviction)
	if err != nil {
		return fmt.Errorf("failed to evict pod %s/%s: %w", pod.Namespace, pod.Name, err)
	}
	return nil
}

// drainNode evicts all non-daemonset pods from a node
func (a *Autoscaler) drainNode(ctx context.Context, nodeName string) error {
	// Get all pods on this node
	pods, err := a.kubeClient.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		FieldSelector: fmt.Sprintf("spec.nodeName=%s", nodeName),
	})
	if err != nil {
		return fmt.Errorf("failed to list pods on node %s: %w", nodeName, err)
	}

	var evictionErrors []error
	for _, pod := range pods.Items {
		// Skip mirror pods (static pods)
		if _, ok := pod.Annotations["kubernetes.io/config.mirror"]; ok {
			continue
		}

		// Skip DaemonSet pods
		for _, ownerRef := range pod.OwnerReferences {
			if ownerRef.Kind == "DaemonSet" {
				continue
			}
		}

		// Skip pods that are already terminating
		if pod.DeletionTimestamp != nil {
			continue
		}

		a.logger.Debug("Evicting pod", "pod", pod.Name, "namespace", pod.Namespace)
		if err := a.evictPod(ctx, pod); err != nil {
			evictionErrors = append(evictionErrors, err)
		}
	}

	if len(evictionErrors) > 0 {
		return fmt.Errorf("failed to evict %d pods: %v", len(evictionErrors), evictionErrors[0])
	}

	a.logger.Info("Drained node", "node", nodeName)
	return nil
}

// isNodeDrained checks if all non-daemonset pods have been evicted from a node
func (a *Autoscaler) isNodeDrained(ctx context.Context, nodeName string) (bool, error) {
	pods, err := a.kubeClient.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		FieldSelector: fmt.Sprintf("spec.nodeName=%s", nodeName),
	})
	if err != nil {
		return false, fmt.Errorf("failed to list pods on node %s: %w", nodeName, err)
	}

	for _, pod := range pods.Items {
		// Skip mirror pods (static pods)
		if _, ok := pod.Annotations["kubernetes.io/config.mirror"]; ok {
			continue
		}

		// Skip DaemonSet pods
		isDaemonSet := false
		for _, ownerRef := range pod.OwnerReferences {
			if ownerRef.Kind == "DaemonSet" {
				isDaemonSet = true
				break
			}
		}
		if isDaemonSet {
			continue
		}

		// If we find any non-daemonset pod that's not terminating, node is not drained
		if pod.DeletionTimestamp == nil {
			return false, nil
		}
	}

	return true, nil
}

// checkLonghornVolumesHealthy checks if all Longhorn volumes are healthy
// This is important before scale-down to ensure data safety
func (a *Autoscaler) checkLonghornVolumesHealthy(ctx context.Context) (bool, error) {
	// List Longhorn volumes using dynamic client
	// For simplicity, we check via the volumes.longhorn.io CRD
	// In a real implementation, you'd use the Longhorn client

	// For now, we'll check via pods - if there are any volume-related pending pods, volumes aren't healthy
	pods, err := a.kubeClient.CoreV1().Pods("longhorn-system").List(ctx, metav1.ListOptions{})
	if err != nil {
		return false, fmt.Errorf("failed to list longhorn pods: %w", err)
	}

	for _, pod := range pods.Items {
		// Check for any non-running longhorn pods that indicate unhealthy volumes
		if pod.Status.Phase != corev1.PodRunning && pod.Status.Phase != corev1.PodSucceeded {
			// Skip pods that are just starting
			if pod.Status.Phase == corev1.PodPending {
				for _, cond := range pod.Status.Conditions {
					if cond.Type == corev1.PodScheduled && cond.Status == corev1.ConditionFalse {
						a.logger.Debug("Longhorn pod not scheduled", "pod", pod.Name)
						return false, nil
					}
				}
			}
		}
	}

	return true, nil
}

// ptr returns a pointer to the value
func ptr[T any](v T) *T {
	return &v
}
