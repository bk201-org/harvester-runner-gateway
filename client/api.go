package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"time"
)

var resourceID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func decodeResponse[T any](data []byte, kind string) (T, error) {
	var value T
	if err := json.Unmarshal(data, &value); err != nil {
		return value, fmt.Errorf("gateway returned an invalid %s response", kind)
	}
	return value, nil
}

func validateID(id string) error {
	if !resourceID.MatchString(id) {
		return configError("resource IDs must start with an alphanumeric character and contain only letters, digits, '.', '_', or '-'")
	}
	return nil
}

func validateVMResponse(status VMStatus, expectedID string) error {
	if validateID(status.ID) != nil || expectedID != "" && status.ID != expectedID {
		return errors.New("gateway returned an invalid VM status")
	}
	return nil
}

func validateVolumeResponse(status VolumeStatus, expectedID string) error {
	if validateID(status.ID) != nil || expectedID != "" && status.ID != expectedID {
		return errors.New("gateway returned an invalid volume status")
	}
	return nil
}

func (c *Client) Health(ctx context.Context) error {
	_, err := c.Do(ctx, Request{operation: "health", Method: http.MethodGet, Path: []string{"healthz"}, Statuses: []int{http.StatusNoContent}})
	return err
}

func (c *Client) Ready(ctx context.Context) error {
	_, err := c.Do(ctx, Request{operation: "readiness", Method: http.MethodGet, Path: []string{"readyz"}, Statuses: []int{http.StatusNoContent}})
	return err
}

func (c *Client) Quota(ctx context.Context) (Quota, error) {
	data, err := c.Do(ctx, Request{operation: "quota", Method: http.MethodGet, Path: []string{"v1", "quota"}, Auth: true, Statuses: []int{http.StatusOK}})
	if err != nil {
		return Quota{}, err
	}
	return decodeResponse[Quota](data, "quota")
}

func (c *Client) CreateVM(ctx context.Context, request VMRequest, idempotencyKey string) (VMStatus, error) {
	data, err := c.Do(ctx, Request{operation: "create_vm", Method: http.MethodPost, Path: []string{"v1", "vms"}, Body: request,
		IdempotencyKey: idempotencyKey, Auth: true, Statuses: []int{http.StatusOK, http.StatusCreated}})
	if err != nil {
		return VMStatus{}, err
	}
	status, err := decodeResponse[VMStatus](data, "VM status")
	if err != nil {
		return VMStatus{}, err
	}
	if err := validateVMResponse(status, ""); err != nil {
		return VMStatus{}, err
	}
	return status, nil
}

func (c *Client) ListVMs(ctx context.Context) ([]VMStatus, error) {
	data, err := c.Do(ctx, Request{operation: "list_vms", Method: http.MethodGet, Path: []string{"v1", "vms"}, Auth: true, Statuses: []int{http.StatusOK}})
	if err != nil {
		return nil, err
	}
	items, err := decodeResponse[[]VMStatus](data, "VM list")
	if err != nil {
		return nil, err
	}
	for _, item := range items {
		if err := validateVMResponse(item, ""); err != nil {
			return nil, errors.New("gateway returned an invalid VM list")
		}
	}
	return items, nil
}

func (c *Client) GetVM(ctx context.Context, id string) (VMStatus, error) {
	if err := validateID(id); err != nil {
		return VMStatus{}, err
	}
	data, err := c.Do(ctx, Request{operation: "get_vm", Method: http.MethodGet, Path: []string{"v1", "vms", id}, Auth: true, Statuses: []int{http.StatusOK}})
	if err != nil {
		return VMStatus{}, err
	}
	status, err := decodeResponse[VMStatus](data, "VM status")
	if err != nil {
		return VMStatus{}, err
	}
	if err := validateVMResponse(status, id); err != nil {
		return VMStatus{}, err
	}
	return status, nil
}

func (c *Client) DeleteVM(ctx context.Context, id string) error {
	if err := validateID(id); err != nil {
		return err
	}
	_, err := c.Do(ctx, Request{operation: "delete_vm", Method: http.MethodDelete, Path: []string{"v1", "vms", id}, Auth: true, Statuses: []int{http.StatusNoContent}})
	return err
}

func (c *Client) SetVMPower(ctx context.Context, id string, state PowerState) error {
	if err := validateID(id); err != nil {
		return err
	}
	if state != PowerOn && state != PowerOff {
		return configError("power state must be on or off")
	}
	_, err := c.Do(ctx, Request{operation: "set_vm_power", Method: http.MethodPut, Path: []string{"v1", "vms", id, "power"}, Auth: true,
		Body: struct {
			State PowerState `json:"state"`
		}{state}, Statuses: []int{http.StatusAccepted}})
	return err
}

func (c *Client) RebootVM(ctx context.Context, id string) error {
	if err := validateID(id); err != nil {
		return err
	}
	_, err := c.Do(ctx, Request{operation: "reboot_vm", Method: http.MethodPost, Path: []string{"v1", "vms", id, "reboot"}, Auth: true, Statuses: []int{http.StatusAccepted}})
	return err
}

func (c *Client) AttachVolume(ctx context.Context, vmID, volumeID string) error {
	if err := validateID(vmID); err != nil {
		return err
	}
	if err := validateID(volumeID); err != nil {
		return err
	}
	_, err := c.Do(ctx, Request{operation: "attach_volume", Method: http.MethodPut, Path: []string{"v1", "vms", vmID, "volumes", volumeID}, Auth: true, Statuses: []int{http.StatusAccepted}})
	return err
}

func (c *Client) DetachVolume(ctx context.Context, vmID, volumeID string) error {
	if err := validateID(vmID); err != nil {
		return err
	}
	if err := validateID(volumeID); err != nil {
		return err
	}
	_, err := c.Do(ctx, Request{operation: "detach_volume", Method: http.MethodDelete, Path: []string{"v1", "vms", vmID, "volumes", volumeID}, Auth: true, Statuses: []int{http.StatusAccepted}})
	return err
}

func (c *Client) CreateVolume(ctx context.Context, request VolumeRequest, idempotencyKey string) (VolumeStatus, error) {
	data, err := c.Do(ctx, Request{operation: "create_volume", Method: http.MethodPost, Path: []string{"v1", "volumes"}, Body: request,
		IdempotencyKey: idempotencyKey, Auth: true, Statuses: []int{http.StatusOK, http.StatusCreated}})
	if err != nil {
		return VolumeStatus{}, err
	}
	status, err := decodeResponse[VolumeStatus](data, "volume status")
	if err != nil {
		return VolumeStatus{}, err
	}
	if err := validateVolumeResponse(status, ""); err != nil {
		return VolumeStatus{}, err
	}
	return status, nil
}

func (c *Client) ListVolumes(ctx context.Context) ([]VolumeStatus, error) {
	data, err := c.Do(ctx, Request{operation: "list_volumes", Method: http.MethodGet, Path: []string{"v1", "volumes"}, Auth: true, Statuses: []int{http.StatusOK}})
	if err != nil {
		return nil, err
	}
	items, err := decodeResponse[[]VolumeStatus](data, "volume list")
	if err != nil {
		return nil, err
	}
	for _, item := range items {
		if err := validateVolumeResponse(item, ""); err != nil {
			return nil, errors.New("gateway returned an invalid volume list")
		}
	}
	return items, nil
}

func (c *Client) GetVolume(ctx context.Context, id string) (VolumeStatus, error) {
	if err := validateID(id); err != nil {
		return VolumeStatus{}, err
	}
	data, err := c.Do(ctx, Request{operation: "get_volume", Method: http.MethodGet, Path: []string{"v1", "volumes", id}, Auth: true, Statuses: []int{http.StatusOK}})
	if err != nil {
		return VolumeStatus{}, err
	}
	status, err := decodeResponse[VolumeStatus](data, "volume status")
	if err != nil {
		return VolumeStatus{}, err
	}
	if err := validateVolumeResponse(status, id); err != nil {
		return VolumeStatus{}, err
	}
	return status, nil
}

func (c *Client) DeleteVolume(ctx context.Context, id string) error {
	if err := validateID(id); err != nil {
		return err
	}
	_, err := c.Do(ctx, Request{operation: "delete_volume", Method: http.MethodDelete, Path: []string{"v1", "volumes", id}, Auth: true, Statuses: []int{http.StatusNoContent}})
	return err
}

// WaitForVMReady polls until the VM reports ready or ctx ends. The caller
// controls the overall timeout through ctx.
func (c *Client) WaitForVMReady(ctx context.Context, id string, interval time.Duration) (VMStatus, error) {
	if err := validateID(id); err != nil {
		return VMStatus{}, err
	}
	if interval <= 0 {
		return VMStatus{}, configError("poll interval must be positive")
	}
	var last VMStatus
	for {
		if err := ctx.Err(); err != nil {
			return VMStatus{}, vmWaitError(id, last, err)
		}
		status, err := c.GetVM(ctx, id)
		if err != nil {
			if ctx.Err() != nil {
				return VMStatus{}, vmWaitError(id, last, ctx.Err())
			}
			return VMStatus{}, fmt.Errorf("wait for VM %s readiness (last phase=%s, IP addresses=%v): %w", id, last.Phase, last.IPAddresses, err)
		}
		if status.ID != "" && status.ID != id {
			return VMStatus{}, errors.New("gateway returned VM status for a different resource")
		}
		last = status
		if status.Ready {
			return status, nil
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return VMStatus{}, vmWaitError(id, last, ctx.Err())
		case <-timer.C:
		}
	}
}

func vmWaitError(id string, status VMStatus, err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("VM %s did not become ready (last phase=%s, IP addresses=%v); VM remains allocated: %w",
			id, status.Phase, status.IPAddresses, err)
	}
	return fmt.Errorf("waiting for VM %s readiness was canceled (last phase=%s, IP addresses=%v); VM remains allocated: %w",
		id, status.Phase, status.IPAddresses, err)
}
