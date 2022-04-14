package hardware_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tinkerbell/hegel/hardware"
	"k8s.io/client-go/tools/clientcmd"
)

func TestKubernetesClient(t *testing.T) {
	config, err := clientcmd.BuildConfigFromFlags("", "/Users/cpd/.kube/config")
	if err != nil {
		panic(err)
	}

	client := hardware.NewKubernetesOrDie(config)
	client.WaitForCacheSync(context.Background())

	hw, err := client.ByIP(context.Background(), "10.10.10.10")
	assert.NoError(t, err)

	data, err := hw.Export()
	require.NoError(t, err)

	fmt.Printf("%s\n", data)
}
