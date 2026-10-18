// Package kube holds the operator's Kubernetes-facing helpers: resolving a
// PersistentVolume to the node that serves it, and talking to the statfs-guard
// agent running on that node.
package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/zzhkhyl/statfs-guard-operator/internal/agent"
	"github.com/zzhkhyl/statfs-guard-operator/internal/metrics"
)

// CephDriver is the CSI driver whose volumes are affected by the CephFS
// statfs staleness described in README.md ("一、它修的是什么").
const CephDriver = "cephfs.csi.ceph.com"

// SubvolumePathAttribute is the CSI volume attribute carrying the CephFS path
// of the subvolume backing a PV.
const SubvolumePathAttribute = "subvolumePath"

// AgentClient reaches the per-node agents.
//
// Requests go through the API server pod proxy rather than to pod IPs
// directly, so the operator does not depend on pod-network reachability or on
// a NetworkPolicy that permits it.
type AgentClient struct {
	Client    kubernetes.Interface
	Namespace string
	Port      int
	Selector  string
	Timeout   time.Duration
}

// NewAgentClient builds a client with defaults applied.
func NewAgentClient(cs kubernetes.Interface, namespace, selector string, port int, timeout time.Duration) *AgentClient {
	if namespace == "" {
		namespace = "statfs-guard"
	}
	if selector == "" {
		selector = "app.kubernetes.io/name=statfs-guard-agent"
	}
	if port == 0 {
		port = 9101
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &AgentClient{Client: cs, Namespace: namespace, Port: port, Selector: selector, Timeout: timeout}
}

// AgentPodOn returns the running agent pod scheduled on node.
func (c *AgentClient) AgentPodOn(ctx context.Context, node string) (corev1.Pod, error) {
	if node == "" {
		return corev1.Pod{}, fmt.Errorf("empty node name")
	}
	list, err := c.Client.CoreV1().Pods(c.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: c.Selector,
		FieldSelector: "spec.nodeName=" + node,
	})
	if err != nil {
		return corev1.Pod{}, fmt.Errorf("list agent pods on %s: %w", node, err)
	}
	ready := make([]corev1.Pod, 0, len(list.Items))
	for _, p := range list.Items {
		if p.DeletionTimestamp != nil {
			continue
		}
		if !podReady(&p) {
			continue
		}
		ready = append(ready, p)
	}
	if len(ready) == 0 {
		return corev1.Pod{}, fmt.Errorf("no ready %s pod on node %s in namespace %s", c.Selector, node, c.Namespace)
	}
	sort.Slice(ready, func(i, j int) bool { return ready[i].Name < ready[j].Name })
	return ready[0], nil
}

// Probe asks the agent on node for one volume, and gets fresh ground truth.
//
// This call is also the repair: the agent samples statfs(2) first and then
// reads ceph.dir.rbytes, which revalidates the node's kernel client cache
// against the auth MDS. The returned Sample reports the drift as it was
// *before* the revalidation, plus how long kubelet has been wrong.
func (c *AgentClient) Probe(ctx context.Context, node, pv string) (agent.Sample, error) {
	started := time.Now()
	pod, err := c.AgentPodOn(ctx, node)
	if err != nil {
		metrics.AgentUp.WithLabelValues(node).Set(0)
		metrics.AgentRequestSeconds.WithLabelValues(node, "probe").Observe(time.Since(started).Seconds())
		return agent.Sample{}, err
	}
	metrics.AgentUp.WithLabelValues(node).Set(1)

	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()

	raw, err := c.Client.CoreV1().RESTClient().Get().
		Namespace(c.Namespace).
		Resource("pods").
		Name(pod.Name+":"+strconv.Itoa(c.Port)).
		SubResource("proxy").
		Suffix("v1/probe").
		Param("pv", pv).
		Do(ctx).Raw()
	metrics.AgentRequestSeconds.WithLabelValues(node, "probe").Observe(time.Since(started).Seconds())
	if err != nil {
		return agent.Sample{}, fmt.Errorf("call agent on %s: %w", node, err)
	}

	var s agent.Sample
	if err := json.Unmarshal(raw, &s); err != nil {
		return agent.Sample{}, fmt.Errorf("decode agent response: %w", err)
	}
	return s, nil
}

// Volumes asks the agent on node which PVs it can see.
func (c *AgentClient) Volumes(ctx context.Context, node string) (map[string]string, error) {
	pod, err := c.AgentPodOn(ctx, node)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()

	raw, err := c.Client.CoreV1().RESTClient().Get().
		Namespace(c.Namespace).
		Resource("pods").
		Name(pod.Name + ":" + strconv.Itoa(c.Port)).
		SubResource("proxy").
		Suffix("v1/volumes").
		Do(ctx).Raw()
	if err != nil {
		return nil, fmt.Errorf("call agent on %s: %w", node, err)
	}
	out := map[string]string{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode agent response: %w", err)
	}
	return out, nil
}

func podReady(p *corev1.Pod) bool {
	if p.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, cond := range p.Status.Conditions {
		if cond.Type == corev1.PodReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}

// IsCephFSVolume reports whether a PV is a CephFS CSI volume.
func IsCephFSVolume(pv *corev1.PersistentVolume) bool {
	return pv != nil && pv.Spec.CSI != nil && pv.Spec.CSI.Driver == CephDriver
}

// SubvolumePath returns the CephFS path backing a PV, if the CSI attributes
// expose one.
func SubvolumePath(pv *corev1.PersistentVolume) string {
	if !IsCephFSVolume(pv) {
		return ""
	}
	return pv.Spec.CSI.VolumeAttributes[SubvolumePathAttribute]
}

// NotFound reports whether the error means "object does not exist".
func NotFound(err error) bool { return apierrors.IsNotFound(err) }
