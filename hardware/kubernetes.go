package hardware

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/pkg/errors"
	tinkv1alpha1 "github.com/tinkerbell/tink/pkg/apis/core/v1alpha1"
	tink "github.com/tinkerbell/tink/pkg/controllers"
	"k8s.io/client-go/rest"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
)

var _ Client = &KubernetesClient{}

type ListerClient interface {
	List(ctx context.Context, list crclient.ObjectList, opts ...crclient.ListOption) error
}

// KubernetesClient is a hardware client backed by a KubernetesClient cluster that contains hardware resources.
type KubernetesClient struct {
	client           ListerClient
	close            func()
	waitForCacheSync func(context.Context) bool
}

// NewKubernetesClientOrDie creates a new KubernetesClient client. It panics upon error.
func NewKubernetesClientOrDie(config *rest.Config, namespace string) *KubernetesClient {
	client, err := NewKubernetesClient(config, namespace)
	if err != nil {
		panic(err)
	}
	return client
}

// NewKubernetesClient creates a new KubernetesClient client instance. It launches a goroutine to perform synchronization between
// the cluster and internal caches. Consumers can wait for the initial sync using WaitForCachesync().
// See k8s.io/client-go/tools/clientcmd for constructing *rest.Config objects.
func NewKubernetesClient(config *rest.Config, namespace string) (*KubernetesClient, error) {
	opts := tink.GetServerOptions()
	opts.Namespace = namespace

	manager, err := tink.NewManager(config, opts)
	if err != nil {
		return nil, err
	}

	managerCtx, cancel := context.WithCancel(context.Background())
	go func() {
		if err := manager.Start(managerCtx); err != nil {
			panic(err)
		}
	}()

	client := NewKubernetesClientWithClient(manager.GetClient())
	client.close = cancel
	client.waitForCacheSync = manager.GetCache().WaitForCacheSync

	return client, nil
}

// NewKubernetesClientWithClient creates a new KubernetesClient instance that uses client to find resources. The
// Close() and WaitForCacheSync() methods of the returned client are noops.
func NewKubernetesClientWithClient(client ListerClient) *KubernetesClient {
	return &KubernetesClient{
		client:           client,
		close:            func() {},
		waitForCacheSync: func(context.Context) bool { return true },
	}
}

// Close stops synchronization with the cluster. Subsequent calls to all of k's methods have undefined behavior.
func (k *KubernetesClient) Close() {
	k.close()
}

// WaitForCacheSync waits for the internal client cache to synchronize.
func (k *KubernetesClient) WaitForCacheSync(ctx context.Context) bool {
	return k.waitForCacheSync(ctx)
}

// IsHealthy always returns true.
func (k *KubernetesClient) IsHealthy(context.Context) bool {
	return true
}

// ByIP retrieves a hardware resource associated with ip.
func (k *KubernetesClient) ByIP(ctx context.Context, ip string) (Hardware, error) {
	var hw tinkv1alpha1.HardwareList
	err := k.client.List(ctx, &hw, crclient.MatchingFields{
		tink.HardwareIPAddrIndex: ip,
	})

	if err != nil {
		return nil, err
	}

	if len(hw.Items) == 0 {
		return nil, fmt.Errorf("no hardware with ip '%v'", ip)
	}

	if len(hw.Items) > 1 {
		names := make([]string, len(hw.Items))
		for i, item := range hw.Items {
			names[i] = item.Name
		}
		return nil, fmt.Errorf("multiple hardware with ip '%v': [%v]", ip, strings.Join(names, ", "))
	}

	return &hardware{hw.Items[0]}, nil
}

// Watch is unimplemented.
func (k *KubernetesClient) Watch(context.Context, string) (Watcher, error) {
	return nil, errors.New("KubernetesClient client: watch is unimplemented")
}

type hardware struct {
	tinkv1alpha1.Hardware
}

func (h hardware) Export() ([]byte, error) {
	marshalled, err := json.Marshal(h.Hardware.Spec)
	if err != nil {
		return nil, err
	}

	if h.Hardware.Spec.Metadata != nil {
		// Marshall and unmarshal to a map so we can marshal the metadata as an escaped json string. This is to remain
		// compatible with existing behavior from the Tinkerbell client in this package.
		tmp := make(map[string]interface{})
		if err = json.Unmarshal(marshalled, &tmp); err != nil {
			return nil, err
		}

		marshalledMetadata, err := json.Marshal(h.Hardware.Spec.Metadata)
		if err != nil {
			return nil, err
		}

		tmp["metadata"] = string(marshalledMetadata)

		marshalled, err = json.Marshal(tmp)
		if err != nil {
			return nil, err
		}
	}

	return marshalled, err
}

func (h hardware) ID() (string, error) {
	return h.Spec.Metadata.Instance.ID, nil
}
