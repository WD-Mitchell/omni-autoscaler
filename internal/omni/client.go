package omni

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/cosi-project/runtime/pkg/safe"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/siderolabs/omni/client/pkg/client"
	"github.com/siderolabs/omni/client/pkg/omni/resources"
	"github.com/siderolabs/omni/client/pkg/omni/resources/omni"
)

// Client wraps the Omni API client for autoscaler operations
type Client struct {
	client      *client.Client
	clusterName string
}

// NewClient creates a new Omni client
func NewClient(endpoint, clusterName string) (*Client, error) {
	// Get service account key from environment
	serviceAccountKey := os.Getenv("OMNI_SERVICE_ACCOUNT_KEY")
	if serviceAccountKey == "" {
		return nil, fmt.Errorf("OMNI_SERVICE_ACCOUNT_KEY environment variable not set")
	}

	opts := []client.Option{
		client.WithServiceAccount(serviceAccountKey),
	}

	c, err := client.New(endpoint, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create Omni client: %w", err)
	}

	return &Client{
		client:      c,
		clusterName: clusterName,
	}, nil
}

// Close closes the Omni client connection
func (c *Client) Close() error {
	return c.client.Close()
}

// MachineSetStatus represents the current state of a machine set
type MachineSetStatus struct {
	Name         string
	MachineCount int
	ReadyCount   int
}

// getMachineCount extracts machine count from MachineAllocation or deprecated MachineClass
func getMachineCount(spec interface{ GetMachineAllocation() interface{ GetMachineCount() uint32 }; GetMachineClass() interface{ GetMachineCount() uint32 } }) (uint32, bool) {
	// Check MachineAllocation first (new field)
	if alloc := spec.GetMachineAllocation(); alloc != nil {
		return alloc.GetMachineCount(), true
	}
	// Fall back to deprecated MachineClass
	if mc := spec.GetMachineClass(); mc != nil {
		return mc.GetMachineCount(), true
	}
	return 0, false
}

// GetMachineSetStatus gets the current status of a machine set
func (c *Client) GetMachineSetStatus(ctx context.Context, machineSetName string) (*MachineSetStatus, error) {
	st := c.client.Omni().State()

	// Get the machine set
	machineSetID := fmt.Sprintf("%s-%s", c.clusterName, machineSetName)

	ms, err := safe.StateGet[*omni.MachineSet](ctx, st, omni.NewMachineSet(resources.DefaultNamespace, machineSetID).Metadata())
	if err != nil {
		return nil, fmt.Errorf("failed to get machine set %s: %w", machineSetID, err)
	}

	spec := ms.TypedSpec().Value

	// Get machine count - check MachineAllocation first (new), then MachineClass (deprecated)
	var machineCount uint32
	if spec.MachineAllocation != nil {
		machineCount = spec.MachineAllocation.MachineCount
	} else if spec.MachineClass != nil {
		machineCount = spec.MachineClass.MachineCount
	}

	return &MachineSetStatus{
		Name:         machineSetName,
		MachineCount: int(machineCount),
	}, nil
}

// ScaleMachineSet scales a machine set to the specified count
func (c *Client) ScaleMachineSet(ctx context.Context, machineSetName string, count int) error {
	st := c.client.Omni().State()

	machineSetID := fmt.Sprintf("%s-%s", c.clusterName, machineSetName)

	// Get current machine set
	ms, err := safe.StateGet[*omni.MachineSet](ctx, st, omni.NewMachineSet(resources.DefaultNamespace, machineSetID).Metadata())
	if err != nil {
		return fmt.Errorf("failed to get machine set %s: %w", machineSetID, err)
	}

	// Update the machine count - check MachineAllocation first (new), then MachineClass (deprecated)
	spec := ms.TypedSpec().Value
	if spec.MachineAllocation != nil {
		spec.MachineAllocation.MachineCount = uint32(count)
	} else if spec.MachineClass != nil {
		spec.MachineClass.MachineCount = uint32(count)
	} else {
		return fmt.Errorf("machine set %s has no machine allocation configured", machineSetID)
	}

	// Update the machine set
	if err := st.Update(ctx, ms, state.WithUpdateOwner(ms.Metadata().Owner())); err != nil {
		return fmt.Errorf("failed to update machine set %s: %w", machineSetID, err)
	}

	return nil
}

// ListMachineSets lists all machine sets for the cluster
func (c *Client) ListMachineSets(ctx context.Context) ([]*MachineSetStatus, error) {
	st := c.client.Omni().State()

	// List all machine sets
	list, err := safe.StateListAll[*omni.MachineSet](ctx, st)
	if err != nil {
		return nil, fmt.Errorf("failed to list machine sets: %w", err)
	}

	var result []*MachineSetStatus
	prefix := c.clusterName + "-"

	for iter := list.Iterator(); iter.Next(); {
		ms := iter.Value()
		id := ms.Metadata().ID()

		// Filter by cluster name prefix
		if strings.HasPrefix(id, prefix) {
			spec := ms.TypedSpec().Value

			// Get machine count - check MachineAllocation first (new), then MachineClass (deprecated)
			var machineCount uint32
			if spec.MachineAllocation != nil {
				machineCount = spec.MachineAllocation.MachineCount
			} else if spec.MachineClass != nil {
				machineCount = spec.MachineClass.MachineCount
			}

			result = append(result, &MachineSetStatus{
				Name:         strings.TrimPrefix(id, prefix),
				MachineCount: int(machineCount),
			})
		}
	}

	return result, nil
}
