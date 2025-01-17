package kubernetes

import (
	"context"
	"fmt"
	v1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiRes "k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/cli-runtime/pkg/resource"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kyma-incubator/reconciler/pkg/logger"
	"github.com/kyma-incubator/reconciler/pkg/test"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	namespace          = "kubeclient-test"
	containerBaseImage = "alpine"
)

var createdResources = make(map[string]metav1.Time, 10)

func TestPatchReplace(t *testing.T) {
	test.IntegrationTest(t)

	t.Parallel()

	//create client
	kubeClient, err := NewKubernetesClient(test.ReadKubeconfig(t), logger.NewLogger(true), nil)
	require.NoError(t, err)

	deleteNamespace(t, kubeClient, true)
	defer deleteNamespace(t, kubeClient, false)

	for idxManifest, manifest := range []string{
		"manifest-before.yaml",
		"manifest-after.yaml",
		"manifest-before.yaml",
		"manifest-after.yaml"} {
		unstructs, err := ToUnstructured(test.ReadFile(t, filepath.Join("test", manifest)), true)
		require.NoError(t, err)

		var expectedImage = containerBaseImage
		var expectedLimits int64 = 100
		if idxManifest%2 == 1 { //each second update updates the image from 'alpine' to 'alpine:3.14'
			expectedImage = fmt.Sprintf("%s:3.14", expectedImage)
			expectedLimits = expectedLimits * 2
		}

		for idxUnstruct, unstruct := range unstructs {
			t.Run(
				fmt.Sprintf("Applying %s", unstruct.GetName()),
				newTestFunc(kubeClient, unstruct, fmt.Sprintf("%d_%d", idxManifest, idxUnstruct), expectedImage, expectedLimits))
		}
	}

}

func newTestFunc(kubeClient Client, unstruct *unstructured.Unstructured, label, expectedImage string, expectedSize int64) func(t *testing.T) {
	return func(t *testing.T) {
		t.Logf("Applying %s and setting label %s", unstruct.GetKind(), label)
		unstruct.SetLabels(map[string]string{"applied": label})
		clientAdapter := kubeClient.(*kubeClientAdapter)
		info, err := clientAdapter.convertToInfo(unstruct, "")
		require.NoError(t, err)
		_, err = clientAdapter.deployResources(context.TODO(), []*resource.Info{info}, []*resource.Info{info})
		require.NoError(t, err)

		k8sResourceUnstruct, err := clientAdapter.Get(unstruct.GetKind(), unstruct.GetName(), unstruct.GetNamespace())
		require.NoError(t, err)

		//verify label
		require.Equal(t, label, k8sResourceUnstruct.GetLabels()["applied"])

		//verify that resource wasn't re-created
		timestamp, ok := createdResources[k8sResourceUnstruct.GetKind()]
		if ok {
			require.Equalf(t, timestamp, createdResources[k8sResourceUnstruct.GetKind()],
				"resource %s with name '%s' got re-created", k8sResourceUnstruct.GetKind(), k8sResourceUnstruct.GetName())
		} else {
			createdResources[k8sResourceUnstruct.GetKind()] = k8sResourceUnstruct.GetCreationTimestamp()
		}

		//verify resource specific attributes
		switch strings.ToLower(k8sResourceUnstruct.GetKind()) {
		case "serviceaccount":
			//verify that not multiple service-accounts were created during reconciliation
			clientSet, err := kubeClient.Clientset()
			require.NoError(t, err)
			saList, err := clientSet.CoreV1().ServiceAccounts(unstruct.GetNamespace()).List(context.Background(), metav1.ListOptions{
				LabelSelector: fmt.Sprintf("applied=%s", label),
			})
			require.NoError(t, err)
			require.Len(t, saList.Items, 1)
		case "statefulset":
			//check image
			var statefulSet v1.StatefulSet
			err = runtime.DefaultUnstructuredConverter.
				FromUnstructured(unstruct.UnstructuredContent(), &statefulSet)
			require.NoError(t, err)
			for _, container := range statefulSet.Spec.Template.Spec.Containers {
				if requireImageName(t, expectedImage, container) {
					return
				}
			}
		case "deployment":
			//check image
			var deployment v1.Deployment
			err = runtime.DefaultUnstructuredConverter.
				FromUnstructured(unstruct.UnstructuredContent(), &deployment)
			require.NoError(t, err)
			for _, container := range deployment.Spec.Template.Spec.Containers {
				require.True(t, container.Resources.Limits.Memory().Equal(apiRes.MustParse(fmt.Sprintf("%dMi", expectedSize))))
				require.True(t, container.Resources.Limits.Cpu().Equal(apiRes.MustParse(fmt.Sprintf("%dm", expectedSize))))
				if requireImageName(t, expectedImage, container) {
					return
				}
			}
		}
	}
}

func requireImageName(t *testing.T, expectedImage string, container corev1.Container) bool {
	if strings.HasPrefix(container.Image, containerBaseImage) {
		t.Logf("Found container image '%s'", expectedImage)
		require.Equal(t, expectedImage, container.Image)
		return true
	}
	return false
}

func deleteNamespace(t *testing.T, kubeClient Client, ignoreError bool) {
	clientSet, err := kubeClient.Clientset()
	require.NoError(t, err)
	err = clientSet.CoreV1().Namespaces().Delete(context.Background(), namespace, metav1.DeleteOptions{})
	if !ignoreError {
		require.NoError(t, err)
	}
	ticker := time.NewTicker(1 * time.Second)
	timeout := time.After(15 * time.Second)
	select {
	case <-ticker.C:
		_, err := clientSet.CoreV1().Namespaces().Get(context.Background(), namespace, metav1.GetOptions{})
		if errors.IsNotFound(err) {
			t.Logf("Namespace '%s' deleted", namespace)
			break
		}
	case <-timeout:
		t.Logf("Timeout reached: namespace '%s' not deleted within expected time range", namespace)
		break
	}
}
