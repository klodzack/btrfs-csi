package driver

import (
	"context"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	coreclient "k8s.io/client-go/kubernetes/typed/core/v1"
	storageclient "k8s.io/client-go/kubernetes/typed/storage/v1"
	"k8s.io/client-go/rest"
)

type kubernetesPVCClient struct {
	core    coreclient.CoreV1Interface
	storage storageclient.StorageV1Interface
}

// NewInClusterPVCClient enables Kubernetes lookups without requiring them for standalone CSI clients.
func NewInClusterPVCClient(mode string) (PVCClient, error) {
	switch mode {
	case "disabled":
		return nil, nil
	case "auto", "enabled":
	default:
		return nil, fmt.Errorf("invalid mode %q: expected auto, enabled, or disabled", mode)
	}
	config, err := rest.InClusterConfig()
	if errors.Is(err, rest.ErrNotInCluster) && mode == "auto" {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load in-cluster credentials: %w", err)
	}
	config.Timeout = 10 * time.Second
	config.UserAgent = driverName
	return newKubernetesPVCClient(config)
}

func newKubernetesPVCClient(config *rest.Config) (PVCClient, error) {
	core, err := coreclient.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("create core Kubernetes client: %w", err)
	}
	storage, err := storageclient.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("create storage Kubernetes client: %w", err)
	}
	return &kubernetesPVCClient{core: core, storage: storage}, nil
}

func (k *kubernetesPVCClient) GetPVC(ctx context.Context,
	namespace, name string) (*corev1.PersistentVolumeClaim, error) {
	return k.core.PersistentVolumeClaims(namespace).Get(ctx, name, metav1.GetOptions{})
}

func (k *kubernetesPVCClient) GetPV(ctx context.Context, name string) (*corev1.PersistentVolume, error) {
	return k.core.PersistentVolumes().Get(ctx, name, metav1.GetOptions{})
}

func (k *kubernetesPVCClient) ListPVs(ctx context.Context) ([]corev1.PersistentVolume, error) {
	var all []corev1.PersistentVolume
	opts := metav1.ListOptions{Limit: 500}
	for {
		page, err := k.core.PersistentVolumes().List(ctx, opts)
		if err != nil {
			return nil, err
		}
		all = append(all, page.Items...)
		opts.Continue = page.Continue
		if opts.Continue == "" {
			return all, nil
		}
	}
}

func (k *kubernetesPVCClient) GetStorageClass(ctx context.Context, name string) (*storagev1.StorageClass, error) {
	return k.storage.StorageClasses().Get(ctx, name, metav1.GetOptions{})
}

func (k *kubernetesPVCClient) Warning(ctx context.Context, pvc *corev1.PersistentVolumeClaim, message string) error {
	now := metav1.Now()
	_, err := k.core.Events(pvc.Namespace).Create(ctx, &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "btrfs-options-", Namespace: pvc.Namespace},
		InvolvedObject: corev1.ObjectReference{
			APIVersion: "v1", Kind: "PersistentVolumeClaim", Namespace: pvc.Namespace, Name: pvc.Name, UID: pvc.UID,
		},
		Type: corev1.EventTypeWarning, Reason: "VolumeOptionsUpdateFailed", Message: message,
		Source: corev1.EventSource{Component: driverName}, Count: 1, FirstTimestamp: now, LastTimestamp: now,
	}, metav1.CreateOptions{})
	return err
}
