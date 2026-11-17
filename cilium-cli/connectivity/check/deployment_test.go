// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

// This file was not reviewed by a human before submission

package check

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/cilium/cilium/cilium-cli/defaults"
	"github.com/cilium/cilium/cilium-cli/k8s"
	"github.com/cilium/cilium/cilium-cli/utils/features"
	"github.com/cilium/cilium/pkg/annotation"
)

func newFakeConnectivityTest(t *testing.T, objects ...runtime.Object) (*ConnectivityTest, *k8s.Client) {
	t.Helper()

	client := &k8s.Client{
		Clientset: fake.NewSimpleClientset(objects...),
	}

	ct := &ConnectivityTest{
		params: Parameters{
			TestNamespace:        "default-test-namespace",
			Writer:               &bytes.Buffer{},
			NamespaceLabels:      map[string]string{"suite": "connectivity"},
			NamespaceAnnotations: map[string]string{"owner": "test"},
			CurlImage:            "quay.io/cilium/alpine-curl:latest",
		},
		Features: features.Set{
			features.DefaultGlobalNamespace: {Enabled: false},
		},
		clients: &deploymentClients{
			src: client,
			dst: client,
		},
		echoPods: make(map[string]Pod),
	}

	return ct, client
}

func echoPod(name string, phase corev1.PodPhase, reason string, ips ...string) *corev1.Pod {
	podIPs := make([]corev1.PodIP, 0, len(ips))
	for _, ip := range ips {
		podIPs = append(podIPs, corev1.PodIP{IP: ip})
	}

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default-test-namespace",
			Labels:    map[string]string{"kind": kindEchoName},
		},
		Status: corev1.PodStatus{Phase: phase, Reason: reason, PodIPs: podIPs},
	}
}

func TestRegisterEchoPodsSkipsSupersededLeftovers(t *testing.T) {
	ct, _ := newFakeConnectivityTest(t,
		echoPod("echo-other-node-live", corev1.PodRunning, "", "10.0.0.1"),
		echoPod("echo-other-node-evicted", corev1.PodFailed, "Evicted"),
	)

	require.NoError(t, ct.registerEchoPods(context.Background()))
	require.Len(t, ct.EchoPods(), 1)
	assert.Contains(t, ct.EchoPods(), "echo-other-node-live")
}

func TestDeployNamespaceCreatesMissingNamespace(t *testing.T) {
	ct, client := newFakeConnectivityTest(t)

	err := ct.deployNamespace(context.Background(), client, "created-ns")
	require.NoError(t, err)

	namespace, err := client.GetNamespace(context.Background(), "created-ns", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "created-ns", namespace.Name)
	assert.Equal(t, "connectivity", namespace.Labels["suite"])
	assert.Equal(t, "cilium-cli", namespace.Labels["app.kubernetes.io/name"])
	assert.Equal(t, "test", namespace.Annotations["owner"])
	assert.Equal(t, "true", namespace.Annotations[annotation.GlobalNamespace])
}

func TestDeployNamespaceUpdatesExistingNamespace(t *testing.T) {
	existing := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "existing-ns",
			Annotations: map[string]string{"existing": "annotation"},
			Labels:      map[string]string{"existing": "label"},
		},
	}
	ct, client := newFakeConnectivityTest(t, existing)

	err := ct.deployNamespace(context.Background(), client, "existing-ns")
	require.NoError(t, err)

	namespace, err := client.GetNamespace(context.Background(), "existing-ns", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "annotation", namespace.Annotations["existing"])
	assert.Equal(t, "true", namespace.Annotations[annotation.GlobalNamespace])
	assert.Equal(t, "label", namespace.Labels["existing"])
}

func TestDeployNamespaceUsesProvidedNamespaceName(t *testing.T) {
	ct, client := newFakeConnectivityTest(t)
	ct.params.TestNamespace = "wrong-ns"

	err := ct.deployNamespace(context.Background(), client, "actual-ns")
	require.NoError(t, err)

	_, err = client.GetNamespace(context.Background(), "actual-ns", metav1.GetOptions{})
	require.NoError(t, err)

	_, err = client.GetNamespace(context.Background(), "wrong-ns", metav1.GetOptions{})
	assert.Error(t, err)
}

func TestDeployCCNPTestEnvCreatesNamespacesAndDeployments(t *testing.T) {
	ct, client := newFakeConnectivityTest(t)

	err := ct.deployCCNPTestEnv(context.Background())
	require.NoError(t, err)

	for _, namespaceName := range []string{ccnpTestNamespace1, ccnpTestNamespace2} {
		namespace, err := client.GetNamespace(context.Background(), namespaceName, metav1.GetOptions{})
		require.NoError(t, err)
		assert.Equal(t, "true", namespace.Annotations[annotation.GlobalNamespace])

		serviceAccount, err := client.GetServiceAccount(context.Background(), namespaceName, ccnpDeploymentName, metav1.GetOptions{})
		require.NoError(t, err)
		assert.Equal(t, ccnpDeploymentName, serviceAccount.Name)

		deployment, err := client.GetDeployment(context.Background(), namespaceName, ccnpDeploymentName, metav1.GetOptions{})
		require.NoError(t, err)
		assert.Equal(t, ccnpDeploymentName, deployment.Name)
		assert.Equal(t, kindCCNPName, deployment.Labels["kind"])
	}
}

func ciliumAgentPod(name string, terminating bool) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "kube-system",
			Labels:    map[string]string{"k8s-app": "cilium"},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	if terminating {
		pod.DeletionTimestamp = &metav1.Time{}
	}
	return pod
}

func newFakeCiliumPodsTest(t *testing.T, src, dst *k8s.Client) *ConnectivityTest {
	t.Helper()
	ct, _ := newFakeConnectivityTest(t)
	ct.params.CiliumNamespace = "kube-system"
	ct.params.AgentPodSelector = defaults.AgentPodSelector
	ct.clients = &deploymentClients{src: src, dst: dst}
	ct.ciliumPods = make(map[string]Pod)
	return ct
}

func TestInitCiliumPodsSkipsTerminating(t *testing.T) {
	client := &k8s.Client{Clientset: fake.NewSimpleClientset(
		ciliumAgentPod("cilium-running", false),
		ciliumAgentPod("cilium-terminating", true),
	)}
	ct := newFakeCiliumPodsTest(t, client, client)

	require.NoError(t, ct.initCiliumPods(context.Background()))
	require.Len(t, ct.ciliumPods, 1)
	assert.Contains(t, ct.ciliumPods, "cilium-running")

	list := client.Clientset.(*fake.Clientset).Actions()[0].(k8stesting.ListAction)
	assert.Equal(t, "status.phase=Running", list.GetListRestrictions().Fields.String())
}

func TestInitCiliumPodsErrorsWithoutUsablePods(t *testing.T) {
	client := &k8s.Client{Clientset: fake.NewSimpleClientset(
		ciliumAgentPod("cilium-terminating-1", true),
		ciliumAgentPod("cilium-terminating-2", true),
	)}
	ct := newFakeCiliumPodsTest(t, client, client)

	require.ErrorIs(t, ct.initCiliumPods(context.Background()), errNoCiliumPods)
}

func TestInitCiliumPodsErrorsWhenOneClusterHasNoUsablePods(t *testing.T) {
	src := &k8s.Client{Clientset: fake.NewSimpleClientset(ciliumAgentPod("cilium-src-running", false))}
	dst := &k8s.Client{Clientset: fake.NewSimpleClientset(ciliumAgentPod("cilium-dst-terminating", true))}
	ct := newFakeCiliumPodsTest(t, src, dst)

	require.ErrorIs(t, ct.initCiliumPods(context.Background()), errNoCiliumPods)
}
