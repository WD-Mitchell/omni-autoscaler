package controller

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
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
	}
}

// Run starts the autoscaler reconciliation loop
func (a *Autoscaler) Run(ctx context.Context) error {
	a.logger.Info("Starting autoscaler", "syncInterval", a.config.SyncInterval)

	ticker := time.NewTicker(a.config.SyncInterval)
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
	a.logger.Debug("Starting reconciliation")

	// Get pending pods
	pendingPods, err := a.getPendingPods(ctx)
	if err != nil {
		return fmt.Errorf("failed to get pending pods: %w", err)
	}

	// Get node utilization
	nodeUtil, err := a.getNodeUtilization(ctx)
	if err != nil {
		return fmt.Errorf("failed to get node utilization: %w", err)
	}

	// Check each configured machine set
	for _, msConfig := range a.config.MachineSets {
		if err := a.reconcileMachineSet(ctx, msConfig, len(pendingPods), nodeUtil); err != nil {
			a.logger.Error("Failed to reconcile machine set", "machineSet", msConfig.Name, "error", err)
		}
	}

	return nil
}

// reconcileMachineSet handles scaling decisions for a single machine set
func (a *Autoscaler) reconcileMachineSet(ctx context.Context, msConfig config.MachineSetConfig, pendingPodCount int, nodeUtil float64) error {
	// Get current machine set status
	status, err := a.omniClient.GetMachineSetStatus(ctx, msConfig.Name)
	if err != nil {
		return fmt.Errorf("failed to get machine set status: %w", err)
	}

	currentCount := status.MachineCount
	desiredCount := currentCount

	a.logger.Debug("Evaluating machine set",
		"machineSet", msConfig.Name,
		"currentCount", currentCount,
		"pendingPods", pendingPodCount,
		"nodeUtilization", fmt.Sprintf("%.2f%%", nodeUtil*100),
	)

	// Check if we need to scale up
	if pendingPodCount >= msConfig.ScaleUpPendingPods && currentCount < msConfig.MaxSize {
		if a.canScaleUp(msConfig.Name) {
			// Scale up by 1 (or more based on pending pods)
			increment := min((pendingPodCount/msConfig.ScaleUpPendingPods), msConfig.MaxSize-currentCount)
			if increment < 1 {
				increment = 1
			}
			desiredCount = currentCount + increment
			desiredCount = min(desiredCount, msConfig.MaxSize)

			a.logger.Info("Scaling up",
				"machineSet", msConfig.Name,
				"from", currentCount,
				"to", desiredCount,
				"reason", fmt.Sprintf("%d pending pods", pendingPodCount),
			)
		} else {
			a.logger.Debug("Scale-up on cooldown", "machineSet", msConfig.Name)
		}
	}

	// Check if we need to scale down
	if nodeUtil < msConfig.ScaleDownUtilization && currentCount > msConfig.MinSize && pendingPodCount == 0 {
		if a.canScaleDown(msConfig.Name) {
			desiredCount = currentCount - 1
			desiredCount = max(desiredCount, msConfig.MinSize)

			a.logger.Info("Scaling down",
				"machineSet", msConfig.Name,
				"from", currentCount,
				"to", desiredCount,
				"reason", fmt.Sprintf("utilization %.2f%% below threshold %.2f%%", nodeUtil*100, msConfig.ScaleDownUtilization*100),
			)
		} else {
			a.logger.Debug("Scale-down on cooldown", "machineSet", msConfig.Name)
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
func (a *Autoscaler) getPendingPods(ctx context.Context) ([]corev1.Pod, error) {
	pods, err := a.kubeClient.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		FieldSelector: "status.phase=Pending",
	})
	if err != nil {
		return nil, err
	}

	var pendingPods []corev1.Pod
	for _, pod := range pods.Items {
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

// getNodeUtilization calculates the average CPU/memory utilization across all nodes
func (a *Autoscaler) getNodeUtilization(ctx context.Context) (float64, error) {
	nodes, err := a.kubeClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return 0, err
	}

	if len(nodes.Items) == 0 {
		return 0, nil
	}

	// Get all pods to calculate resource usage
	pods, err := a.kubeClient.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		FieldSelector: "status.phase=Running",
	})
	if err != nil {
		return 0, err
	}

	// Calculate requested resources per node
	nodeRequests := make(map[string]resource.Quantity)
	for _, pod := range pods.Items {
		if pod.Spec.NodeName == "" {
			continue
		}
		for _, container := range pod.Spec.Containers {
			if cpu := container.Resources.Requests.Cpu(); cpu != nil {
				current := nodeRequests[pod.Spec.NodeName]
				current.Add(*cpu)
				nodeRequests[pod.Spec.NodeName] = current
			}
		}
	}

	// Calculate average utilization
	var totalUtil float64
	var workerCount int

	for _, node := range nodes.Items {
		// Skip control plane nodes
		if _, ok := node.Labels["node-role.kubernetes.io/control-plane"]; ok {
			continue
		}

		allocatable := node.Status.Allocatable.Cpu()
		if allocatable.IsZero() {
			continue
		}

		requested := nodeRequests[node.Name]
		util := float64(requested.MilliValue()) / float64(allocatable.MilliValue())
		totalUtil += util
		workerCount++
	}

	if workerCount == 0 {
		return 0, nil
	}

	return totalUtil / float64(workerCount), nil
}

// canScaleUp checks if scale-up is allowed based on cooldown
func (a *Autoscaler) canScaleUp(machineSet string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	lastScale, ok := a.lastScaleUp[machineSet]
	if !ok {
		return true
	}
	return time.Since(lastScale) >= a.config.Cooldowns.ScaleUp
}

// canScaleDown checks if scale-down is allowed based on cooldown
func (a *Autoscaler) canScaleDown(machineSet string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	// Also check if we recently scaled up
	if lastUp, ok := a.lastScaleUp[machineSet]; ok {
		if time.Since(lastUp) < a.config.Cooldowns.ScaleDown {
			return false
		}
	}

	lastScale, ok := a.lastScaleDown[machineSet]
	if !ok {
		return true
	}
	return time.Since(lastScale) >= a.config.Cooldowns.ScaleDown
}
