/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

		http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package provider

import (
	"fmt"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/k8s-stackdriver/custom-metrics-stackdriver-adapter/pkg/config"
	"github.com/kubernetes-incubator/custom-metrics-apiserver/pkg/provider"
	stackdriver "google.golang.org/api/monitoring/v3"
	v1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/klog"
	"k8s.io/metrics/pkg/apis/custom_metrics"
	"k8s.io/metrics/pkg/apis/external_metrics"
)

// TODO(kawych):
// * Support long responses from Stackdriver (pagination).

type clock interface {
	Now() time.Time
}

type realClock struct{}

func (c realClock) Now() time.Time {
	return time.Now()
}

// StackdriverProvider is a provider of custom metrics from Stackdriver.
type StackdriverProvider struct {
	kubeClient          *corev1.CoreV1Client
	stackdriverService  *stackdriver.Service
	config              *config.GceConfig
	rateInterval        time.Duration
	translator          *Translator
	useNewResourceModel bool
}

// NewStackdriverProvider creates a StackdriverProvider
func NewStackdriverProvider(kubeClient *corev1.CoreV1Client, mapper apimeta.RESTMapper, stackdriverService *stackdriver.Service, rateInterval, alignmentPeriod time.Duration, useNewResourceModel bool) provider.MetricsProvider {
	gceConf, err := config.GetGceConfig()
	if err != nil {
		klog.Fatalf("Failed to retrieve GCE config: %v", err)
	}

	return &StackdriverProvider{
		kubeClient:         kubeClient,
		stackdriverService: stackdriverService,
		config:             gceConf,
		rateInterval:       rateInterval,
		translator: &Translator{
			service:             stackdriverService,
			config:              gceConf,
			reqWindow:           rateInterval,
			alignmentPeriod:     alignmentPeriod,
			clock:               realClock{},
			mapper:              mapper,
			useNewResourceModel: useNewResourceModel,
		},
	}
}

// GetMetricByName fetches a particular metric for a particular object.
// The namespace will be empty if the metric is root-scoped.
func (p *StackdriverProvider) GetMetricByName(name types.NamespacedName, info provider.CustomMetricInfo) (*custom_metrics.MetricValue, error) {
	if name.Namespace == "" {
		return p.getRootScopedMetricByName(info.GroupResource, name.Name, info.Metric)
	}
	return p.getNamespacedMetricByName(info.GroupResource, name.Namespace, name.Name, info.Metric)
}

// GetMetricBySelector fetches a particular metric for a set of objects matching
// the given label selector.  The namespace will be empty if the metric is root-scoped.
func (p *StackdriverProvider) GetMetricBySelector(namespace string, selector labels.Selector, info provider.CustomMetricInfo) (*custom_metrics.MetricValueList, error) {
	if namespace == "" {
		return p.getRootScopedMetricBySelector(info.GroupResource, selector, info.Metric)
	}
	return p.getNamespacedMetricBySelector(info.GroupResource, namespace, selector, info.Metric)
}

// getRootScopedMetricByName queries Stackdriver for metrics identified by name and not associated
// with any namespace. Current implementation doesn't support root scoped metrics.
func (p *StackdriverProvider) getRootScopedMetricByName(groupResource schema.GroupResource, name string, escapedMetricName string) (*custom_metrics.MetricValue, error) {
	if !p.translator.useNewResourceModel {
		return nil, NewOperationNotSupportedError("Get root scoped metric by name")
	}
	if groupResource.Resource != nodeResource {
		return nil, NewOperationNotSupportedError(fmt.Sprintf("Get root scoped metric by name for resource %q", groupResource.Resource))
	}
	matchingNode, err := p.kubeClient.Nodes().Get(name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	metricKind, err := p.translator.GetMetricKind(getCustomMetricName(escapedMetricName))
	if err != nil {
		return nil, err
	}
	stackdriverRequest, err := p.translator.GetSDReqForNodes(&v1.NodeList{Items: []v1.Node{*matchingNode}}, getCustomMetricName(escapedMetricName), metricKind)
	if err != nil {
		return nil, err
	}
	stackdriverResponse, err := stackdriverRequest.Do()
	if err != nil {
		return nil, err
	}
	return p.translator.GetRespForSingleObject(stackdriverResponse, groupResource, escapedMetricName, "", name)
}

// getRootScopedMetricBySelector queries Stackdriver for metrics identified by selector and not
// associated with any namespace. Current implementation doesn't support root scoped metrics.
func (p *StackdriverProvider) getRootScopedMetricBySelector(groupResource schema.GroupResource, selector labels.Selector, escapedMetricName string) (*custom_metrics.MetricValueList, error) {
	if !p.translator.useNewResourceModel {
		return nil, NewOperationNotSupportedError("Get root scoped metric by selector")
	}
	if groupResource.Resource != nodeResource {
		return nil, NewOperationNotSupportedError(fmt.Sprintf("Get root scoped metric by selector for resource %q", groupResource.Resource))
	}
	matchingNodes, err := p.kubeClient.Nodes().List(metav1.ListOptions{LabelSelector: selector.String()})
	if err != nil {
		return nil, err
	}
	metricKind, err := p.translator.GetMetricKind(getCustomMetricName(escapedMetricName))
	if err != nil {
		return nil, err
	}
	result := []custom_metrics.MetricValue{}
	for i := 0; i < len(matchingNodes.Items); i += oneOfMax {
		nodesSlice := &v1.NodeList{Items: matchingNodes.Items[i:min(i+oneOfMax, len(matchingNodes.Items))]}
		stackdriverRequest, err := p.translator.GetSDReqForNodes(nodesSlice, getCustomMetricName(escapedMetricName), metricKind)
		if err != nil {
			return nil, err
		}
		stackdriverResponse, err := stackdriverRequest.Do()
		if err != nil {
			return nil, err
		}
		slice, err := p.translator.GetRespForMultipleObjects(stackdriverResponse, p.translator.getNodeItems(matchingNodes), groupResource, escapedMetricName)
		if err != nil {
			return nil, err
		}
		result = append(result, slice...)
	}
	return &custom_metrics.MetricValueList{Items: result}, nil
}

// GetNamespacedMetricByName queries Stackdriver for metrics identified by name and associated
// with a namespace.
func (p *StackdriverProvider) getNamespacedMetricByName(groupResource schema.GroupResource, namespace string, name string, escapedMetricName string) (*custom_metrics.MetricValue, error) {
	if groupResource.Resource != podResource {
		return nil, NewOperationNotSupportedError(fmt.Sprintf("Get namespaced metric by name for resource %q", groupResource.Resource))
	}
	matchingPod, err := p.kubeClient.Pods(namespace).Get(name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	metricKind, err := p.translator.GetMetricKind(getCustomMetricName(escapedMetricName))
	if err != nil {
		return nil, err
	}
	stackdriverRequest, err := p.translator.GetSDReqForPods(&v1.PodList{Items: []v1.Pod{*matchingPod}}, getCustomMetricName(escapedMetricName), metricKind, namespace)
	if err != nil {
		return nil, err
	}
	stackdriverResponse, err := stackdriverRequest.Do()
	if err != nil {
		return nil, err
	}
	return p.translator.GetRespForSingleObject(stackdriverResponse, groupResource, escapedMetricName, namespace, name)
}

// GetNamespacedMetricBySelector queries Stackdriver for metrics identified by selector and associated
// with a namespace.
func (p *StackdriverProvider) getNamespacedMetricBySelector(groupResource schema.GroupResource, namespace string, labelSelector labels.Selector, escapedMetricName string) (*custom_metrics.MetricValueList, error) {
	if groupResource.Resource != podResource {
		return nil, NewOperationNotSupportedError(fmt.Sprintf("Get namespaced metric by selector for resource %q", groupResource.Resource))
	}
	matchingPods, err := p.kubeClient.Pods(namespace).List(metav1.ListOptions{LabelSelector: labelSelector.String()})
	if err != nil {
		return nil, err
	}
	metricKind, err := p.translator.GetMetricKind(getCustomMetricName(escapedMetricName))
	if err != nil {
		return nil, err
	}
	result := []custom_metrics.MetricValue{}
	for i := 0; i < len(matchingPods.Items); i += oneOfMax {
		podsSlice := &v1.PodList{Items: matchingPods.Items[i:min(i+oneOfMax, len(matchingPods.Items))]}
		stackdriverRequest, err := p.translator.GetSDReqForPods(podsSlice, getCustomMetricName(escapedMetricName), metricKind, namespace)
		if err != nil {
			return nil, err
		}
		stackdriverResponse, err := stackdriverRequest.Do()
		if err != nil {
			return nil, err
		}
		slice, err := p.translator.GetRespForMultipleObjects(stackdriverResponse, p.translator.getPodItems(matchingPods), groupResource, escapedMetricName)
		if err != nil {
			return nil, err
		}
		result = append(result, slice...)
	}
	return &custom_metrics.MetricValueList{Items: result}, nil
}

// ListAllMetrics returns all custom metrics available from Stackdriver.
// List only pod metrics
func (p *StackdriverProvider) ListAllMetrics() []provider.CustomMetricInfo {
	metrics := []provider.CustomMetricInfo{}
	stackdriverRequest := p.translator.ListMetricDescriptors()
	response, err := stackdriverRequest.Do()
	if err != nil {
		klog.Errorf("Failed request to stackdriver api: %s", err)
		return metrics
	}
	return p.translator.GetMetricsFromSDDescriptorsResp(response)
}

// GetExternalMetric queries Stackdriver for external metrics.
func (p *StackdriverProvider) GetExternalMetric(namespace string, metricSelector labels.Selector, info provider.ExternalMetricInfo) (*external_metrics.ExternalMetricValueList, error) {
	metricNameEscaped := info.Metric
	metricName := getExternalMetricName(metricNameEscaped)
	metricKind, err := p.translator.GetMetricKind(metricName)
	if err != nil {
		return nil, err
	}
	stackdriverRequest, err := p.translator.GetExternalMetricRequest(metricName, metricKind, metricSelector)
	if err != nil {
		return nil, err
	}
	stackdriverResponse, err := stackdriverRequest.Do()
	if err != nil {
		return nil, err
	}
	externalMetricItems, err := p.translator.GetRespForExternalMetric(stackdriverResponse, metricNameEscaped)
	if err != nil {
		return nil, err
	}
	return &external_metrics.ExternalMetricValueList{
		Items: externalMetricItems,
	}, nil
}

// ListAllExternalMetrics returns a list of available external metrics.
// Not implemented (currently returns empty list).
func (p *StackdriverProvider) ListAllExternalMetrics() []provider.ExternalMetricInfo {
	return []provider.ExternalMetricInfo{}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func getExternalMetricName(metricName string) string {
	return strings.Replace(metricName, "|", "/", -1)
}

func getCustomMetricName(metricName string) string {
	if strings.Contains(metricName, "|") {
		return getExternalMetricName(metricName)
	}
	return "custom.googleapis.com/" + metricName
}
